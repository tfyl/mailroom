package zoho

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- MessageWriter ---

// Send posts a message. Zoho takes recipients as comma-separated strings and the body as a
// field, so there is no MIME to assemble — unlike Gmail, which wants raw RFC 5322.
func (p *Provider) Send(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	body := p.composeBody(out)
	// Attachments are stored before the message is composed, so a file Zoho would not take
	// stops the send rather than travelling missing from it. See upload.go.
	if err := p.attachUploads(ctx, out, body); err != nil {
		return mmail.ScopedID{}, err
	}

	var result struct {
		MessageID flexString `json:"messageId"`
		FolderID  flexString `json:"folderId"`
	}
	if err := p.do(ctx, http.MethodPost, "/accounts/"+p.accountID+"/messages", nil, body, &result); err != nil {
		return mmail.ScopedID{}, err
	}

	// Zoho answers a send with a messageId and no folderId, and every id here is
	// <folder>/<message> — so handing back what the response contained produced
	// "/1234567890123456791", which splitNative rejects as malformed. A caller that sent a
	// message and then tried to read it, label it or reply to it got an error naming its own
	// id, for a message that had been sent perfectly well.
	//
	// The message is in Sent, so the folder is looked up rather than invented. If that lookup
	// fails there is no honest id to give back: an id that cannot address the message is
	// worse than none, because it is only discovered at the point somebody uses it.
	folder := result.FolderID.String()
	if folder == "" {
		found, err := p.systemFolderID(ctx, folderSent)
		if err != nil {
			return mmail.ScopedID{}, fmt.Errorf("the message was sent, but its id could not be "+
				"resolved: Zoho reports no folder on a send and the Sent folder could not be "+
				"read: %w", err)
		}
		folder = found
	}
	return p.scoped(folder, result.MessageID.String()), nil
}

// composeBody renders an outgoing message as the fields Zoho's compose endpoint takes.
//
// Shared with CreateDraft, which posts to the same endpoint with one extra field, so that a
// draft and the send it becomes cannot end up carrying different recipients or a different
// body because one of the two was changed and the other was not.
func (p *Provider) composeBody(out mmail.Outgoing) map[string]any {
	body := map[string]any{
		"fromAddress": p.account.Address,
		"toAddress":   joinAddresses(out.To),
		"subject":     out.Subject,
	}
	if cc := joinAddresses(out.Cc); cc != "" {
		body["ccAddress"] = cc
	}
	if bcc := joinAddresses(out.Bcc); bcc != "" {
		body["bccAddress"] = bcc
	}
	if out.Body.HTML != "" {
		body["content"], body["mailFormat"] = out.Body.HTML, "html"
	} else {
		body["content"], body["mailFormat"] = out.Body.Text, "plaintext"
	}
	if !out.InReplyTo.Zero() {
		// Zoho threads a reply by being told which message it answers, rather than by
		// In-Reply-To headers the caller assembles.
		//
		// Unverified for a draft. Zoho documents this pair on the send path and documents
		// inReplyTo and refHeader on the save-draft one, and those two take the RFC 5322
		// Message-ID header rather than the numeric id a ScopedID carries — reaching one
		// costs a second request to /header, and mailroom does not hold it. So a draft reply
		// is composed the way a sent reply is, and whether Zoho threads it that way when the
		// message is only being saved has not been confirmed against a mailbox. The failure
		// if it does not is a draft that is saved and not threaded, which is visible to
		// whoever opens it, rather than mail going anywhere unexpected.
		if _, messageID, err := splitNative(out.InReplyTo.Native); err == nil {
			body["action"] = "reply"
			body["referencedMessageId"] = messageID
		}
	}
	return body
}

// The two system folders this provider has to resolve by name. Zoho's folder listing
// documents the system set as Inbox, Drafts, Templates, Snoozed, Sent, Spam, Trash and
// Outbox — https://www.zoho.com/mail/help/api/get-all-folder-details.html.
const (
	folderSent   = "Sent"
	folderDrafts = "Drafts"
)

// systemFolderID finds the folder Zoho files a particular kind of message in — Sent for a
// message that has gone, Drafts for one that has not.
//
// Matched on Zoho's own isSystemFolder plus the name, because a user folder can be called
// anything including "Sent" or "Drafts", and picking theirs would hand back an id addressing
// the wrong folder — which resolves, and is wrong, the worst combination.
//
// The name match without the flag is the fallback rather than the rule, and it earns its
// place: Zoho's published folder sample carries a folderType and no isSystemFolder field at
// all, while the live mailbox this provider was built against answers with isSystemFolder.
// Leaning on the flag alone would leave the lookup failing on any account that answers the
// way the documentation does.
func (p *Provider) systemFolderID(ctx context.Context, name string) (string, error) {
	var folders []struct {
		FolderID   flexString `json:"folderId"`
		FolderName string     `json:"folderName"`
		IsSystem   bool       `json:"isSystemFolder"`
	}
	if err := p.get(ctx, "/accounts/"+p.accountID+"/folders", nil, &folders); err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.IsSystem && strings.EqualFold(f.FolderName, name) {
			return f.FolderID.String(), nil
		}
	}
	for _, f := range folders {
		if strings.EqualFold(f.FolderName, name) {
			return f.FolderID.String(), nil
		}
	}
	return "", fmt.Errorf("no %s folder in this mailbox", name)
}

// --- LabelManager ---

// ListLabels merges Zoho's two separate concepts into the one model.
//
// Folders are exclusive — a message lives in exactly one, so applying a folder moves it.
// Labels are not. The Exclusive flag is what lets a single mail_modify serve both, and lets
// Gmail, which has only the non-exclusive kind, use the same path.
func (p *Provider) ListLabels(ctx context.Context) ([]mmail.Label, error) {
	var folders []struct {
		FolderID   flexString `json:"folderId"`
		FolderName string     `json:"folderName"`
		Unread     int        `json:"unreadCount"`
		Total      int        `json:"messageCount"`
		IsSystem   bool       `json:"isSystemFolder"`
	}
	if err := p.get(ctx, "/accounts/"+p.accountID+"/folders", nil, &folders); err != nil {
		return nil, err
	}

	var labels []struct {
		LabelID     flexString `json:"labelId"`
		DisplayName string     `json:"displayName"`
	}
	if err := p.get(ctx, "/accounts/"+p.accountID+"/labels", nil, &labels); err != nil {
		return nil, err
	}

	out := make([]mmail.Label, 0, len(folders)+len(labels))
	for _, f := range folders {
		kind := mmail.LabelUser
		if f.IsSystem {
			kind = mmail.LabelSystem
		}
		out = append(out, mmail.Label{
			ID: mmail.LabelID("folder:" + f.FolderID.String()), Name: f.FolderName, Kind: kind,
			Exclusive: true, Unread: f.Unread, Total: f.Total,
		})
	}
	for _, l := range labels {
		out = append(out, mmail.Label{
			ID: mmail.LabelID("label:" + l.LabelID.String()), Name: l.DisplayName,
			Kind: mmail.LabelUser, Exclusive: false,
		})
	}
	return out, nil
}

func (p *Provider) CreateLabel(ctx context.Context, name string, exclusive bool) (mmail.Label, error) {
	if exclusive {
		var created struct {
			FolderID   flexString `json:"folderId"`
			FolderName string     `json:"folderName"`
		}
		body := map[string]any{"folderName": name}
		if err := p.do(ctx, http.MethodPost, "/accounts/"+p.accountID+"/folders", nil, body, &created); err != nil {
			return mmail.Label{}, err
		}
		return mmail.Label{
			ID: mmail.LabelID("folder:" + created.FolderID.String()), Name: created.FolderName,
			Kind: mmail.LabelUser, Exclusive: true,
		}, nil
	}

	var created struct {
		LabelID     flexString `json:"labelId"`
		DisplayName string     `json:"displayName"`
	}
	body := map[string]any{"displayName": name, "color": "#5a9bd5"}
	if err := p.do(ctx, http.MethodPost, "/accounts/"+p.accountID+"/labels", nil, body, &created); err != nil {
		return mmail.Label{}, err
	}
	return mmail.Label{
		ID: mmail.LabelID("label:" + created.LabelID.String()), Name: created.DisplayName,
		Kind: mmail.LabelUser, Exclusive: false,
	}, nil
}

func (p *Provider) DeleteLabel(ctx context.Context, id mmail.LabelID) error {
	kind, native, err := splitLabelID(id)
	if err != nil {
		return err
	}
	switch kind {
	case "folder":
		return p.do(ctx, http.MethodDelete, "/accounts/"+p.accountID+"/folders/"+native, nil, nil, nil)
	default:
		return p.do(ctx, http.MethodDelete, "/accounts/"+p.accountID+"/labels/"+native, nil, nil, nil)
	}
}

// ApplyLabels adds and removes labels, and moves messages between folders.
//
// Applying an exclusive label is a move, so at most one may be applied per call: asking to
// put a message in two folders at once is not a thing that can be honoured, and silently
// picking one would be worse than refusing.
func (p *Provider) ApplyLabels(ctx context.Context, ids []mmail.ScopedID, add, remove []mmail.LabelID) error {
	if len(ids) == 0 {
		return nil
	}
	messageIDs, err := nativeMessageIDs(ids)
	if err != nil {
		return err
	}

	var folderMoves []string
	for _, id := range add {
		kind, native, err := splitLabelID(id)
		if err != nil {
			return err
		}
		if kind == "folder" {
			folderMoves = append(folderMoves, native)
			continue
		}
		if err := p.updateMessages(ctx, messageIDs, map[string]any{
			"mode": "applyLabel", "labelId": native,
		}); err != nil {
			return err
		}
	}
	if len(folderMoves) > 1 {
		return fmt.Errorf("a message can only be in one folder; asked to move it to %d", len(folderMoves))
	}
	if len(folderMoves) == 1 {
		if err := p.updateMessages(ctx, messageIDs, map[string]any{
			"mode": "moveMessage", "destfolderId": folderMoves[0],
		}); err != nil {
			return err
		}
	}

	for _, id := range remove {
		kind, native, err := splitLabelID(id)
		if err != nil {
			return err
		}
		if kind == "folder" {
			// Removing a folder is meaningless: a message is always somewhere. Moving it is
			// the operation, and that is an add.
			continue
		}
		if err := p.updateMessages(ctx, messageIDs, map[string]any{
			"mode": "removeLabel", "labelId": native,
		}); err != nil {
			return err
		}
	}
	return nil
}

// SetFlags writes read state and the follow-up flag, each only when the update names it.
//
// Zoho keeps the two in separate modes of the same endpoint, so an update naming one of them
// is one request rather than two — and, more to the point, a caller marking mail read does
// not have its follow-up flags cleared as a side effect of a mode it never asked for.
func (p *Provider) SetFlags(ctx context.Context, ids []mmail.ScopedID, update mmail.FlagUpdate) error {
	if len(ids) == 0 || update.Empty() {
		return nil
	}
	messageIDs, err := nativeMessageIDs(ids)
	if err != nil {
		return err
	}

	if update.Read != nil {
		mode := "markAsUnread"
		if *update.Read {
			mode = "markAsRead"
		}
		if err := p.updateMessages(ctx, messageIDs, map[string]any{"mode": mode}); err != nil {
			return err
		}
	}

	if update.Starred != nil {
		// setFlag, naming the flag. There is no changeFlag mode — Zoho documents markAsRead,
		// markAsUnread, moveMessage, setFlag, applyLabel, removeLabel, removeAllLabels,
		// archiveMails, unArchiveMails, moveToSpam and markNotSpam, and nothing else. A mode
		// Zoho does not recognise is not a request it performs.
		flagID := flagNameNone
		if *update.Starred {
			flagID = flagNameFollowUp
		}
		if err := p.updateMessages(ctx, messageIDs, map[string]any{"mode": "setFlag", "flagid": flagID}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) updateMessages(ctx context.Context, messageIDs []string, fields map[string]any) error {
	body := map[string]any{"messageId": messageIDs}
	for k, v := range fields {
		body[k] = v
	}
	return p.do(ctx, http.MethodPut, "/accounts/"+p.accountID+"/updatemessage", nil, body, nil)
}

// splitLabelID separates the two namespaces ListLabels merges. Prefixing is what keeps a
// folder id and a label id from colliding once they share one type.
func splitLabelID(id mmail.LabelID) (kind, native string, err error) {
	kind, native, ok := strings.Cut(string(id), ":")
	if !ok || native == "" {
		return "", "", fmt.Errorf("malformed zoho label id %q: want folder:<id> or label:<id>", id)
	}
	if kind != "folder" && kind != "label" {
		return "", "", fmt.Errorf("unknown zoho label namespace %q", kind)
	}
	return kind, native, nil
}

func nativeMessageIDs(ids []mmail.ScopedID) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		_, messageID, err := splitNative(id.Native)
		if err != nil {
			return nil, err
		}
		out = append(out, messageID)
	}
	return out, nil
}

func joinAddresses(addrs []mmail.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = a.Email
	}
	return strings.Join(parts, ",")
}

var _ interface {
	mmail.Provider
	mmail.MessageReader
	mmail.ThreadReader
	mmail.AttachmentReader
	mmail.MessageWriter
	mmail.DraftManager
	mmail.LabelManager
} = (*Provider)(nil)

// EffectOfApplying classifies a Zoho label or folder id.
//
// A label is a sticker: applying one adds to a message and takes nothing away, so no label is
// destructive whatever it is called. A folder is exclusive, and applying one is the
// moveMessage above — which, aimed at the Trash or Spam folder, is the whole of what deleting
// mail means on Zoho. Trash reaches the same bin through Destroyer, so this move is now the
// second route to trashing rather than the only one, and this is what gates it.
//
// Zoho's folder ids are numeric and carry no name, so the folder listing has to be read to
// classify one. It is read once per provider: a modify naming two folders should not fetch the
// same list twice, and a provider is built per call, so the cache cannot go stale within a
// life it might be used across.
// DeletingDestroysMail is true for a folder and false for a label.
//
// Zoho keeps the two apart and DeleteLabel already routes on it: a folder id goes to
// DELETE /accounts/{id}/folders/{native}, which removes the folder and its messages, while a
// label id unfiles mail that keeps existing. Delete destroys mail a message at a time; a
// folder delete is the wider call, taking whatever happens to be filed there.
func (p *Provider) DeletingDestroysMail(_ context.Context, id mmail.LabelID) (bool, error) {
	kind, _, err := splitLabelID(id)
	if err != nil {
		// Not an id this provider can act on, so nothing will be deleted. DeleteLabel
		// refuses it with this same error.
		return false, nil
	}
	return kind == "folder", nil
}

func (p *Provider) EffectOfApplying(ctx context.Context, id mmail.LabelID) (mmail.LabelEffect, error) {
	kind, native, err := splitLabelID(id)
	if err != nil {
		// Not an id this provider can act on. ApplyLabels refuses it with this same error,
		// which is where a caller gets a useful answer; calling it destructive here would be
		// a guess about a string that names nothing.
		return mmail.EffectFile, nil
	}
	if kind != "folder" {
		return mmail.EffectFile, nil
	}

	names, err := p.folderNames(ctx)
	if err != nil {
		return "", err
	}
	return mmail.EffectOfMailboxName(names[native]), nil
}

// folderNames maps Zoho's folder ids to the names people see on them, memoised per provider.
func (p *Provider) folderNames(ctx context.Context) (map[string]string, error) {
	p.foldersMu.Lock()
	defer p.foldersMu.Unlock()
	if p.folders != nil {
		return p.folders, nil
	}

	var folders []struct {
		FolderID   flexString `json:"folderId"`
		FolderName string     `json:"folderName"`
	}
	if err := p.get(ctx, "/accounts/"+p.accountID+"/folders", nil, &folders); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(folders))
	for _, f := range folders {
		names[f.FolderID.String()] = f.FolderName
	}
	p.folders = names
	return names, nil
}
