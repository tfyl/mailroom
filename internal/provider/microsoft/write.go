package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- label namespaces ---
//
// Graph has two things a message can be filed under, and they behave differently enough that
// merging them into one id space would let a delete remove the wrong one. A folder is
// exclusive — parentFolderId is a single value, so putting a message in one takes it out of
// wherever it was — and a category is not. Prefixing keeps them apart once they share a type,
// which is the same problem Zoho's folders and labels have.
//
// A category has no id of its own: Graph identifies it by its display name, both on a message
// and in the master list. So the native part of a category label is that name.

const (
	labelFolder   = "folder"
	labelCategory = "category"
)

func folderLabel(id string) mmail.LabelID  { return mmail.LabelID(labelFolder + ":" + id) }
func categoryLabel(n string) mmail.LabelID { return mmail.LabelID(labelCategory + ":" + n) }

func splitLabelID(id mmail.LabelID) (kind, native string, err error) {
	kind, native, ok := strings.Cut(string(id), ":")
	if !ok || native == "" {
		return "", "", fmt.Errorf("malformed microsoft label id %q: want folder:<id> or category:<name>", id)
	}
	if kind != labelFolder && kind != labelCategory {
		return "", "", fmt.Errorf("unknown microsoft label namespace %q", kind)
	}
	return kind, native, nil
}

// --- LabelManager ---

type mailFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	ChildFolderCount int    `json:"childFolderCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	TotalItemCount   int    `json:"totalItemCount"`
}

// systemFolderNames is how a folder is told apart from one somebody made, and it is a
// heuristic rather than a fact.
//
// Graph puts no well-known flag on the mailFolder resource. The seventeen well-known names —
// inbox, deleteditems, junkemail and the rest — work in a URL path in any locale, but they are
// not a property, and the only thing a listing hands back is a displayName in the mailbox's
// own language. Resolving each name to an id would cost seventeen requests on every call to
// list labels, which is a poor trade for a display distinction.
//
// So this reads English display names, and a mailbox in another language reports its system
// folders as ordinary ones. That is wrong in a way that is visible and harmless; the
// alternative was wrong in a way that was slow.
var systemFolderNames = map[string]bool{
	"Inbox": true, "Drafts": true, "Sent Items": true, "Deleted Items": true,
	"Junk Email": true, "Archive": true, "Outbox": true, "Conversation History": true,
	"Notes": true, "Clutter": true, "Scheduled": true, "Snoozed": true,
	"Conflicts": true, "Sync Issues": true, "Server Failures": true, "Local Failures": true,
}

// maxFolders bounds the walk below. A mailbox with a pathological folder tree should give a
// caller a large answer slowly rather than an unbounded number of requests.
const maxFolders = 500

// ListLabels merges Graph's two filing concepts into the one model.
//
// Folders are walked rather than listed. /me/mailFolders returns only the top level, and a
// mailbox that files anything into a subfolder would otherwise be missing exactly the labels
// somebody wants to apply — so children are fetched for each folder that reports having any.
func (p *Provider) ListLabels(ctx context.Context) ([]mmail.Label, error) {
	folders, err := p.walkFolders(ctx, "/me/mailFolders", 0)
	if err != nil {
		return nil, err
	}

	out := make([]mmail.Label, 0, len(folders))
	for _, f := range folders {
		kind := mmail.LabelUser
		if systemFolderNames[f.DisplayName] {
			kind = mmail.LabelSystem
		}
		out = append(out, mmail.Label{
			ID: folderLabel(f.ID), Name: f.DisplayName, Kind: kind,
			Exclusive: true, Unread: f.UnreadItemCount, Total: f.TotalItemCount,
		})
	}

	categories, err := p.listCategories(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range categories {
		out = append(out, mmail.Label{
			ID: categoryLabel(c), Name: c, Kind: mmail.LabelUser, Exclusive: false,
		})
	}
	return out, nil
}

func (p *Provider) walkFolders(ctx context.Context, path string, depth int) ([]mailFolder, error) {
	// Ten levels is deeper than any mailbox anybody has explained to me, and the bound is what
	// keeps a cycle in the answer from becoming an unbounded walk.
	if depth > 10 {
		return nil, nil
	}

	query := url.Values{}
	query.Set("$top", "100")
	query.Set("$select", "id,displayName,parentFolderId,childFolderCount,unreadItemCount,totalItemCount")

	var page struct {
		Value    []mailFolder `json:"value"`
		NextLink string       `json:"@odata.nextLink"`
	}
	if err := p.get(ctx, path, query, &page); err != nil {
		return nil, err
	}

	out := page.Value
	for page.NextLink != "" && len(out) < maxFolders {
		var next struct {
			Value    []mailFolder `json:"value"`
			NextLink string       `json:"@odata.nextLink"`
		}
		if err := p.follow(ctx, page.NextLink, &next); err != nil {
			return nil, err
		}
		out = append(out, next.Value...)
		page.NextLink = next.NextLink
	}

	var nested []mailFolder
	for _, f := range out {
		if f.ChildFolderCount == 0 || len(out)+len(nested) >= maxFolders {
			continue
		}
		children, err := p.walkFolders(ctx, "/me/mailFolders/"+escapeID(f.ID)+"/childFolders", depth+1)
		if err != nil {
			return nil, err
		}
		nested = append(nested, children...)
	}
	return append(out, nested...), nil
}

func (p *Provider) listCategories(ctx context.Context) ([]string, error) {
	var page struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := p.get(ctx, "/me/outlook/masterCategories", nil, &page); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(page.Value))
	for _, c := range page.Value {
		out = append(out, c.DisplayName)
	}
	return out, nil
}

func (p *Provider) CreateLabel(ctx context.Context, name string, exclusive bool) (mmail.Label, error) {
	if exclusive {
		var created mailFolder
		body := map[string]any{"displayName": name}
		if err := p.do(ctx, http.MethodPost, "/me/mailFolders", nil, body, &created); err != nil {
			return mmail.Label{}, err
		}
		return mmail.Label{
			ID: folderLabel(created.ID), Name: created.DisplayName,
			Kind: mmail.LabelUser, Exclusive: true,
		}, nil
	}

	var created struct {
		DisplayName string `json:"displayName"`
	}
	// A category needs a colour, and Graph names them presetN rather than by value. preset0
	// is the first of the twenty-five Outlook offers; a caller that cares can change it in
	// Outlook, and refusing to create one for want of a colour would be worse.
	body := map[string]any{"displayName": name, "color": "preset0"}
	if err := p.do(ctx, http.MethodPost, "/me/outlook/masterCategories", nil, body, &created); err != nil {
		return mmail.Label{}, err
	}
	return mmail.Label{
		ID: categoryLabel(created.DisplayName), Name: created.DisplayName,
		Kind: mmail.LabelUser, Exclusive: false,
	}, nil
}

func (p *Provider) DeleteLabel(ctx context.Context, id mmail.LabelID) error {
	kind, native, err := splitLabelID(id)
	if err != nil {
		return err
	}
	if kind == labelFolder {
		return p.do(ctx, http.MethodDelete, "/me/mailFolders/"+escapeID(native), nil, nil, nil)
	}
	// The master category list is keyed by the category's own id rather than by its name, so
	// removing one means finding it first.
	catID, err := p.categoryID(ctx, native)
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodDelete, "/me/outlook/masterCategories/"+escapeID(catID), nil, nil, nil)
}

func (p *Provider) categoryID(ctx context.Context, name string) (string, error) {
	var page struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := p.get(ctx, "/me/outlook/masterCategories", nil, &page); err != nil {
		return "", err
	}
	for _, c := range page.Value {
		if strings.EqualFold(c.DisplayName, name) {
			return c.ID, nil
		}
	}
	return "", mmail.ErrNotFound
}

// ApplyLabels moves messages between folders and adds or removes categories.
//
// Applying an exclusive label is a move, so at most one may be applied per call: a message
// cannot be in two folders, and silently picking one of them would be worse than refusing.
//
// Categories are a whole-array property, not an add-and-remove operation, so each message is
// read before it is written. Patching the array a caller asked for without merging would drop
// every category the message already carried, which is a silent loss of somebody's filing.
func (p *Provider) ApplyLabels(ctx context.Context, ids []mmail.ScopedID, add, remove []mmail.LabelID) error {
	if len(ids) == 0 {
		return nil
	}

	var destination string
	addCategories, err := partition(add, &destination)
	if err != nil {
		return err
	}
	removeCategories, err := partition(remove, nil)
	if err != nil {
		return err
	}

	for _, id := range ids {
		if len(addCategories) > 0 || len(removeCategories) > 0 {
			if err := p.recategorise(ctx, id.Native, addCategories, removeCategories); err != nil {
				return err
			}
		}
		if destination != "" {
			body := map[string]any{"destinationId": destination}
			path := "/me/messages/" + escapeID(id.Native) + "/move"
			if err := p.do(ctx, http.MethodPost, path, nil, body, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// partition splits a label list into the categories it names and, when destination is
// non-nil, the single folder it may move to.
//
// A folder in the removal list is dropped rather than refused: a message is always somewhere,
// so removing it from a folder is not an operation — moving it is, and that is an add.
func partition(ids []mmail.LabelID, destination *string) ([]string, error) {
	var categories []string
	for _, id := range ids {
		kind, native, err := splitLabelID(id)
		if err != nil {
			return nil, err
		}
		if kind == labelCategory {
			categories = append(categories, native)
			continue
		}
		if destination == nil {
			continue
		}
		if *destination != "" && *destination != native {
			return nil, fmt.Errorf("a message can only be in one folder; asked to move it to both %q and %q",
				*destination, native)
		}
		*destination = native
	}
	return categories, nil
}

func (p *Provider) recategorise(ctx context.Context, messageID string, add, remove []string) error {
	query := url.Values{}
	query.Set("$select", "id,categories")

	var current message
	if err := p.get(ctx, "/me/messages/"+escapeID(messageID), query, &current); err != nil {
		return err
	}

	merged := make([]string, 0, len(current.Categories)+len(add))
	for _, existing := range current.Categories {
		if !containsFold(remove, existing) {
			merged = append(merged, existing)
		}
	}
	for _, wanted := range add {
		if !containsFold(merged, wanted) {
			merged = append(merged, wanted)
		}
	}

	body := map[string]any{"categories": merged}
	return p.do(ctx, http.MethodPatch, "/me/messages/"+escapeID(messageID), nil, body, nil)
}

func containsFold(haystack []string, needle string) bool {
	for _, v := range haystack {
		if strings.EqualFold(v, needle) {
			return true
		}
	}
	return false
}

// SetFlags maps read and starred onto the two message properties Outlook keeps them in.
//
// Starred is the follow-up flag, which is the nearest thing Outlook has to a star and the same
// mapping Zoho needs. It is written here, filtered on in Search and reported by convert, so
// all three agree — a mapping used by one and not the others is how a starred search comes
// back with messages that then say they are not starred.
//
// A PATCH writes only the properties named in it, so an update that mentions one flag leaves
// the other where it was. Naming both regardless would clear somebody's follow-up flags on
// every request to mark mail read.
func (p *Provider) SetFlags(ctx context.Context, ids []mmail.ScopedID, update mmail.FlagUpdate) error {
	if len(ids) == 0 || update.Empty() {
		return nil
	}
	body := map[string]any{}
	if update.Read != nil {
		body["isRead"] = *update.Read
	}
	if update.Starred != nil {
		status := notFlagged
		if *update.Starred {
			status = flagged
		}
		body["flag"] = map[string]any{"flagStatus": status}
	}
	for _, id := range ids {
		if err := p.do(ctx, http.MethodPatch, "/me/messages/"+escapeID(id.Native), nil, body, nil); err != nil {
			return err
		}
	}
	return nil
}

// --- Destroyer ---

// Trash moves messages to Deleted Items, which is what Outlook itself calls deleting.
func (p *Provider) Trash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.moveAll(ctx, ids, deletedItems)
}

// Untrash moves messages back to the inbox.
//
// Not back to where they came from: Exchange records that a message is in Deleted Items and
// not where it was before, so there is nothing to restore to. The inbox is the honest guess
// and is where Outlook's own restore puts a message whose original folder is unknown. Worth
// knowing before somebody wonders why a recovered message is not back in its project folder.
func (p *Provider) Untrash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.moveAll(ctx, ids, "inbox")
}

// Delete removes a message from the mailbox for good, with no undo and no confirmation, which
// is why reaching it requires the destructive capability rather than labels.
//
// permanentDelete rather than DELETE. Graph does not document where a DELETE puts a message,
// and the existence of a separate permanentDelete — plus the same distinction drawn in words
// between a message rule's `delete` and its `permanentDelete` actions — says plainly that a
// DELETE is the soft one. Presenting that as a permanent delete would tell a caller something
// irreversible had happened when it had not, and leave whoever wanted the mail gone believing
// it was.
//
// "Permanent" is Exchange's word, and it is worth knowing what it means: the message goes to
// the purges folder, out of the reach of the mailbox's owner and of this API, and recoverable
// only by an administrator within the retention window.
func (p *Provider) Delete(ctx context.Context, ids []mmail.ScopedID) error {
	for _, id := range ids {
		path := "/me/messages/" + escapeID(id.Native) + "/permanentDelete"
		if err := p.do(ctx, http.MethodPost, path, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// moveAll moves messages to a folder, which may be named by one of Graph's well-known names
// rather than by an id.
//
// A move answers with the moved message, and that answer is discarded: ordinarily the id in it
// would be a new one, because a move copies and deletes and an ordinary Graph id encodes the
// folder — but every request here asks for immutable ids, and those survive a move within the
// mailbox. Reading the response would mean reporting a change that has not happened.
func (p *Provider) moveAll(ctx context.Context, ids []mmail.ScopedID, destination string) error {
	body := map[string]any{"destinationId": destination}
	for _, id := range ids {
		path := "/me/messages/" + escapeID(id.Native) + "/move"
		if err := p.do(ctx, http.MethodPost, path, nil, body, nil); err != nil {
			return err
		}
	}
	return nil
}

// --- DraftManager ---

// maxInlineAttachment is the size above which an attachment has to be uploaded in its own
// session rather than carried in the message. Graph's documented limit for an attachment sent
// as part of a request is 3 MB; the margin below it is for the base64 expansion and the rest
// of the message travelling in the same request.
const maxInlineAttachment = 3 << 20

func (p *Provider) CreateDraft(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	body, err := p.compose(out)
	if err != nil {
		return mmail.ScopedID{}, err
	}

	var created message
	if err := p.do(ctx, http.MethodPost, "/me/messages", nil, body, &created); err != nil {
		return mmail.ScopedID{}, err
	}
	return p.scoped(created.ID), nil
}

func (p *Provider) UpdateDraft(ctx context.Context, id mmail.ScopedID, out mmail.Outgoing) error {
	body, err := p.compose(out)
	if err != nil {
		return err
	}
	return p.do(ctx, http.MethodPatch, "/me/messages/"+escapeID(id.Native), nil, body, nil)
}

// SendDraft sends a draft and reports the id it was already known by.
//
// Graph answers a send with 202 and an empty body, so there is no new id to report — and with
// immutable ids there is no new id to report: the identifier survives the move into Sent
// Items, which is the whole reason every request here asks for that id type. Handing back a
// fresh-looking id here would be inventing one.
func (p *Provider) SendDraft(ctx context.Context, id mmail.ScopedID) (mmail.ScopedID, error) {
	path := "/me/messages/" + escapeID(id.Native) + "/send"
	if err := p.do(ctx, http.MethodPost, path, nil, nil, nil); err != nil {
		return mmail.ScopedID{}, err
	}
	return id, nil
}

func (p *Provider) DeleteDraft(ctx context.Context, id mmail.ScopedID) error {
	return p.do(ctx, http.MethodDelete, "/me/messages/"+escapeID(id.Native), nil, nil, nil)
}

func (p *Provider) ListDrafts(ctx context.Context, cursor string) (mmail.Page[mmail.Message], error) {
	if cursor != "" {
		var page messagePage
		if err := p.follow(ctx, cursor, &page); err != nil {
			return mmail.Page[mmail.Message]{}, err
		}
		return p.page(page), nil
	}

	query := url.Values{}
	query.Set("$select", listFields)
	query.Set("$top", "50")
	query.Set("$orderby", "lastModifiedDateTime desc")

	var page messagePage
	if err := p.get(ctx, "/me/mailFolders/drafts/messages", query, &page); err != nil {
		return mmail.Page[mmail.Message]{}, err
	}
	return p.page(page), nil
}

// --- MessageWriter ---

// Send delivers a message by creating a draft and sending that, rather than by posting to
// sendMail.
//
// sendMail is one request instead of two, and answers with nothing at all: no id, no way for
// a caller to look at what it just sent, and nothing to put in an audit record beyond "it
// worked". A draft has an id before it goes, and — because every request here asks for
// immutable ids — that id still resolves once the message has moved into Sent Items. The
// extra round trip buys a message the caller can actually refer to afterwards.
//
// A reply is built with createReply, so that Exchange assigns the conversation and writes the
// In-Reply-To and References headers itself. Composing those by hand is what a provider does
// when the service will not do it; this one will, and doing it here would be a second, worse
// implementation of threading sitting next to the authoritative one.
func (p *Provider) Send(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	draft, err := p.draftFor(ctx, out)
	if err != nil {
		return mmail.ScopedID{}, err
	}
	return p.SendDraft(ctx, draft)
}

func (p *Provider) draftFor(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	if out.InReplyTo.Zero() {
		return p.CreateDraft(ctx, out)
	}

	// Posted with no body at all, then patched. createReply accepts a comment or a message
	// body and answers 400 if given both, and the draft it returns can simply be updated —
	// so sending nothing is the one shape that cannot trip over that rule.
	var reply message
	path := "/me/messages/" + escapeID(out.InReplyTo.Native) + "/createReply"
	if err := p.do(ctx, http.MethodPost, path, nil, nil, &reply); err != nil {
		return mmail.ScopedID{}, err
	}
	if reply.ID == "" {
		return mmail.ScopedID{}, fmt.Errorf("Microsoft created a reply draft but reported no id for it")
	}

	// createReply pre-fills the recipients and quotes the original. Whatever the caller
	// supplied wins, and anything it left empty keeps what Exchange chose — so a reply with no
	// explicit recipients still goes back to the sender.
	body, err := p.compose(out)
	if err != nil {
		return mmail.ScopedID{}, err
	}
	delete(body, "subject")
	// A key present with a null value clears the property, so an empty recipient list is
	// removed rather than sent: whatever createReply chose stands unless the caller named
	// somebody. The subject goes for the same reason — Exchange has already written the
	// "RE:" one, and replacing it would break the reply out of its own conversation.
	for field, supplied := range map[string]int{
		"toRecipients":  len(out.To),
		"ccRecipients":  len(out.Cc),
		"bccRecipients": len(out.Bcc),
	} {
		if supplied == 0 {
			delete(body, field)
		}
	}
	if err := p.do(ctx, http.MethodPatch, "/me/messages/"+escapeID(reply.ID), nil, body, nil); err != nil {
		return mmail.ScopedID{}, err
	}
	return p.scoped(reply.ID), nil
}

// maxRecipients is Exchange Online's documented ceiling across To, Cc and Bcc together. Past
// it a send is rejected, and the rejection is a long way from the count that caused it.
const maxRecipients = 500

// compose renders an outgoing message as the JSON body Graph takes for a draft.
func (p *Provider) compose(out mmail.Outgoing) (map[string]any, error) {
	if total := len(out.To) + len(out.Cc) + len(out.Bcc); total > maxRecipients {
		return nil, p.unsupported(mmail.CapSend,
			fmt.Sprintf("sending to %d recipients", total),
			fmt.Sprintf("Exchange Online allows at most %d recipients across to, cc and bcc "+
				"in one message; send it in batches", maxRecipients))
	}

	body := map[string]any{
		"subject":       out.Subject,
		"toRecipients":  asRecipients(out.To),
		"ccRecipients":  asRecipients(out.Cc),
		"bccRecipients": asRecipients(out.Bcc),
	}
	// Graph's request examples spell these HTML and Text and its responses answer in lower
	// case. OData enum values are matched without regard to case, so either is accepted; this
	// follows the documented requests rather than the observed replies.
	if out.Body.HTML != "" {
		body["body"] = map[string]any{"contentType": "HTML", "content": out.Body.HTML}
	} else {
		body["body"] = map[string]any{"contentType": "Text", "content": out.Body.Text}
	}

	if len(out.Attachments) > 0 {
		attachments := make([]map[string]any, 0, len(out.Attachments))
		for _, a := range out.Attachments {
			if len(a.Content) > maxInlineAttachment {
				// Anything larger needs an upload session, which is a several-request dance
				// this does not implement. Refusing names the file, because a caller sending
				// four attachments needs to know which one it has to shrink.
				return nil, p.unsupported(mmail.CapSend,
					fmt.Sprintf("sending %q, which is %d bytes", a.Filename, len(a.Content)),
					fmt.Sprintf("Graph carries an attachment inside a message only up to %d "+
						"bytes; anything larger needs an upload session, which is not "+
						"implemented", maxInlineAttachment))
			}
			attachments = append(attachments, map[string]any{
				"@odata.type":  "#microsoft.graph.fileAttachment",
				"name":         a.Filename,
				"contentType":  a.MimeType,
				"contentBytes": a.Content,
				"isInline":     a.Inline,
			})
		}
		body["attachments"] = attachments
	}
	return body, nil
}

var _ interface {
	mmail.Provider
	mmail.MessageReader
	mmail.ThreadReader
	mmail.AttachmentReader
	mmail.MessageWriter
	mmail.DraftManager
	mmail.LabelManager
	mmail.Destroyer
} = (*Provider)(nil)

// binFolders are Graph's well-known folder names whose contents are on their way out, and
// what moving mail into each one means.
//
// Well-known names rather than display names: Graph puts no flag on a mailFolder saying which
// one is the bin, and a display name is in the mailbox's own language. These names work in a
// URL path in any locale, which makes them the one locale-proof handle Graph offers.
var binFolders = map[string]mmail.LabelEffect{
	deletedItems: mmail.EffectTrash,
	junkEmail:    mmail.EffectSpam,
	// Where a permanentDelete puts a message. Nothing here moves mail into it, but a caller
	// naming it would be asking for exactly that, and it should be refused on the same terms.
	"recoverableitemsdeletions": mmail.EffectTrash,
}

// junkEmail is Graph's well-known name for the Junk Email folder.
const junkEmail = "junkemail"

// EffectOfApplying classifies a Graph label id.
//
// A category is a sticker — applying one moves nothing — so only a folder can be destructive.
// Applying a folder is the move in ApplyLabels above, and aimed at Deleted Items that is
// character for character the request Trash makes.
//
// Two ids reach the same folder and both have to be recognised. Move and copy accept a
// well-known name in place of an id, so `folder:deleteditems` works and is what CreateFilter
// already writes for a rule that deletes; and an ordinary caller reading ListLabels gets the
// mailbox's own opaque id. The well-known names are resolved to those ids once per provider,
// which is the only way to compare the two.
// DeletingDestroysMail is true for a mail folder and false for a category.
//
// Graph draws the line for us and DeleteLabel already acts on it: a folder id goes to
// DELETE /me/mailFolders/{id}, which takes the folder and the mail inside it, while a
// category id goes to the master category list and leaves every message that carried it.
func (p *Provider) DeletingDestroysMail(_ context.Context, id mmail.LabelID) (bool, error) {
	kind, _, err := splitLabelID(id)
	if err != nil {
		// Not an id this provider can act on, so nothing will be deleted. DeleteLabel
		// refuses it with this same error, which is where a caller gets a useful answer.
		return false, nil
	}
	return kind == labelFolder, nil
}

func (p *Provider) EffectOfApplying(ctx context.Context, id mmail.LabelID) (mmail.LabelEffect, error) {
	kind, native, err := splitLabelID(id)
	if err != nil {
		// Not an id this provider can act on. ApplyLabels refuses it with this same error,
		// which is where a caller gets a useful answer; calling it destructive here would be
		// a guess about a string that names nothing.
		return mmail.EffectFile, nil
	}
	if kind != labelFolder {
		return mmail.EffectFile, nil
	}
	if effect, ok := binFolders[strings.ToLower(native)]; ok {
		return effect, nil
	}

	resolved, err := p.binFolderIDs(ctx)
	if err != nil {
		return "", err
	}
	if effect, ok := resolved[native]; ok {
		return effect, nil
	}
	return mmail.EffectFile, nil
}

// binFolderIDs resolves the well-known bins to this mailbox's own folder ids, once per
// provider.
//
// A bin the mailbox does not have is skipped rather than failing the lookup: an account
// without a Junk Email folder is a mailbox somebody can still file mail in, and refusing every
// modify on it would turn a missing folder into an outage. Any other failure is returned, and
// the caller refuses the call on it — a classification that could not be made has not passed.
func (p *Provider) binFolderIDs(ctx context.Context) (map[string]mmail.LabelEffect, error) {
	p.binsMu.Lock()
	defer p.binsMu.Unlock()
	if p.bins != nil {
		return p.bins, nil
	}

	resolved := map[string]mmail.LabelEffect{}
	for name, effect := range binFolders {
		var folder struct {
			ID string `json:"id"`
		}
		query := url.Values{}
		query.Set("$select", "id")
		err := p.get(ctx, "/me/mailFolders/"+name, query, &folder)
		if errors.Is(err, mmail.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if folder.ID != "" {
			resolved[folder.ID] = effect
		}
	}
	p.bins = resolved
	return resolved, nil
}
