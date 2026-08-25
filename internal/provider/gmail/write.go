package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"google.golang.org/api/gmail/v1"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/rfc5322"
)

// --- MessageWriter ---

func (p *Provider) Send(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	msg, err := p.build(ctx, out)
	if err != nil {
		return mmail.ScopedID{}, err
	}
	sent, err := p.svc.Users.Messages.Send("me", msg).Context(ctx).Do()
	if err != nil {
		return mmail.ScopedID{}, p.wrap("send", err)
	}
	return mmail.ScopedID{Account: p.account.ID, Native: sent.Id}, nil
}

// --- DraftManager ---

func (p *Provider) CreateDraft(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	msg, err := p.build(ctx, out)
	if err != nil {
		return mmail.ScopedID{}, err
	}
	draft, err := p.svc.Users.Drafts.Create("me", &gmail.Draft{Message: msg}).Context(ctx).Do()
	if err != nil {
		return mmail.ScopedID{}, p.wrap("create_draft", err)
	}
	return mmail.ScopedID{Account: p.account.ID, Native: draft.Id}, nil
}

func (p *Provider) UpdateDraft(ctx context.Context, id mmail.ScopedID, out mmail.Outgoing) error {
	msg, err := p.build(ctx, out)
	if err != nil {
		return err
	}
	_, err = p.svc.Users.Drafts.Update("me", id.Native, &gmail.Draft{Message: msg}).Context(ctx).Do()
	return p.wrap("update_draft", err)
}

func (p *Provider) SendDraft(ctx context.Context, id mmail.ScopedID) (mmail.ScopedID, error) {
	sent, err := p.svc.Users.Drafts.Send("me", &gmail.Draft{Id: id.Native}).Context(ctx).Do()
	if err != nil {
		return mmail.ScopedID{}, p.wrap("send_draft", err)
	}
	return mmail.ScopedID{Account: p.account.ID, Native: sent.Id}, nil
}

func (p *Provider) DeleteDraft(ctx context.Context, id mmail.ScopedID) error {
	return p.wrap("delete_draft", p.svc.Users.Drafts.Delete("me", id.Native).Context(ctx).Do())
}

func (p *Provider) ListDrafts(ctx context.Context, cursor string) (mmail.Page[mmail.Message], error) {
	call := p.svc.Users.Drafts.List("me").MaxResults(50).Context(ctx)
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	resp, err := call.Do()
	if err != nil {
		return mmail.Page[mmail.Message]{}, p.wrap("list_drafts", err)
	}

	out := make([]mmail.Message, 0, len(resp.Drafts))
	for _, d := range resp.Drafts {
		if d.Message == nil {
			continue
		}
		full, err := p.svc.Users.Messages.Get("me", d.Message.Id).Format("metadata").
			MetadataHeaders("To", "Cc", "Subject", "Date").Context(ctx).Do()
		if err != nil {
			continue
		}
		m := p.convert(full, false)
		// Report the draft id, not the underlying message id: the draft id is what update,
		// send and delete take, and handing back the message id would produce calls that
		// fail with a confusing not-found.
		m.ID = mmail.ScopedID{Account: p.account.ID, Native: d.Id}
		out = append(out, m)
	}
	return mmail.Page[mmail.Message]{Items: out, Cursor: resp.NextPageToken}, nil
}

// --- LabelManager ---

func (p *Provider) ListLabels(ctx context.Context) ([]mmail.Label, error) {
	resp, err := p.svc.Users.Labels.List("me").Context(ctx).Do()
	if err != nil {
		return nil, p.wrap("list_labels", err)
	}
	out := make([]mmail.Label, 0, len(resp.Labels))
	for _, l := range resp.Labels {
		kind := mmail.LabelUser
		if l.Type == "system" {
			kind = mmail.LabelSystem
		}
		out = append(out, mmail.Label{
			ID: mmail.LabelID(l.Id), Name: l.Name, Kind: kind,
			// Gmail labels are never exclusive: a message can carry many at once.
			Exclusive: false,
			Unread:    int(l.MessagesUnread), Total: int(l.MessagesTotal),
		})
	}
	return out, nil
}

func (p *Provider) CreateLabel(ctx context.Context, name string, exclusive bool) (mmail.Label, error) {
	if exclusive {
		// Gmail has no exclusive labels. Refusing is honest; silently creating a normal
		// label would give the caller something that behaves differently from what it asked
		// for, and it would only notice much later.
		return mmail.Label{}, &mmail.UnsupportedError{
			Provider: mmail.ProviderGmail, Account: p.account.Alias,
			Address: p.account.Address, Capability: mmail.CapLabels,
		}
	}
	l, err := p.svc.Users.Labels.Create("me", &gmail.Label{
		Name: name, LabelListVisibility: "labelShow", MessageListVisibility: "show",
	}).Context(ctx).Do()
	if err != nil {
		return mmail.Label{}, p.wrap("create_label", err)
	}
	return mmail.Label{ID: mmail.LabelID(l.Id), Name: l.Name, Kind: mmail.LabelUser}, nil
}

func (p *Provider) DeleteLabel(ctx context.Context, id mmail.LabelID) error {
	return p.wrap("delete_label", p.svc.Users.Labels.Delete("me", string(id)).Context(ctx).Do())
}

func (p *Provider) ApplyLabels(ctx context.Context, ids []mmail.ScopedID, add, remove []mmail.LabelID) error {
	if len(ids) == 0 {
		return nil
	}
	req := &gmail.BatchModifyMessagesRequest{
		Ids:            natives(ids),
		AddLabelIds:    labelStrings(add),
		RemoveLabelIds: labelStrings(remove),
	}
	return p.wrap("apply_labels", p.svc.Users.Messages.BatchModify("me", req).Context(ctx).Do())
}

// SetFlags maps read and starred onto Gmail's label model, where unread and starred are
// labels rather than fields.
//
// Only what the update names is touched. Gmail is the one provider where writing both every
// time would be harmless to express, and doing it anyway would still be wrong: a caller
// marking twenty messages read has not asked for the stars on them to be cleared.
func (p *Provider) SetFlags(ctx context.Context, ids []mmail.ScopedID, update mmail.FlagUpdate) error {
	if len(ids) == 0 || update.Empty() {
		return nil
	}
	var add, remove []mmail.LabelID
	if update.Read != nil {
		if *update.Read {
			remove = append(remove, "UNREAD")
		} else {
			add = append(add, "UNREAD")
		}
	}
	if update.Starred != nil {
		if *update.Starred {
			add = append(add, "STARRED")
		} else {
			remove = append(remove, "STARRED")
		}
	}
	return p.ApplyLabels(ctx, ids, add, remove)
}

// --- Destroyer ---

func (p *Provider) Trash(ctx context.Context, ids []mmail.ScopedID) error {
	for _, id := range ids {
		if _, err := p.svc.Users.Messages.Trash("me", id.Native).Context(ctx).Do(); err != nil {
			return p.wrap("trash", err)
		}
	}
	return nil
}

func (p *Provider) Untrash(ctx context.Context, ids []mmail.ScopedID) error {
	for _, id := range ids {
		if _, err := p.svc.Users.Messages.Untrash("me", id.Native).Context(ctx).Do(); err != nil {
			return p.wrap("untrash", err)
		}
	}
	return nil
}

// Delete removes permanently. Gmail's batchDelete has no undo and no confirmation, which is
// why reaching it requires the `destructive` capability rather than `labels`.
func (p *Provider) Delete(ctx context.Context, ids []mmail.ScopedID) error {
	if len(ids) == 0 {
		return nil
	}
	req := &gmail.BatchDeleteMessagesRequest{Ids: natives(ids)}
	return p.wrap("delete", p.svc.Users.Messages.BatchDelete("me", req).Context(ctx).Do())
}

// build renders an outgoing message, resolving the sending address and threading headers.
func (p *Provider) build(ctx context.Context, out mmail.Outgoing) (*gmail.Message, error) {
	from := p.account.Address
	if from == "" {
		profile, err := p.svc.Users.GetProfile("me").Context(ctx).Do()
		if err != nil {
			return nil, p.wrap("get_profile", err)
		}
		from = profile.EmailAddress
	}

	var reply *rfc5322.ReplyContext
	var threadID string
	if !out.InReplyTo.Zero() {
		ctxHeaders, err := p.replyHeaders(ctx, out.InReplyTo)
		if err != nil {
			return nil, err
		}
		reply, threadID = ctxHeaders, ctxHeaders.ThreadID
	}

	raw, err := rfc5322.Compose(out, from, reply)
	if err != nil {
		return nil, fmt.Errorf("composing message: %w", err)
	}
	msg := &gmail.Message{Raw: base64.URLEncoding.EncodeToString(raw)}
	if threadID != "" {
		msg.ThreadId = threadID
	}
	return msg, nil
}

// replyHeaders reads the Message-ID and References of the message being replied to, so the
// reply threads properly in the recipient's client rather than starting a new conversation.
func (p *Provider) replyHeaders(ctx context.Context, id mmail.ScopedID) (*rfc5322.ReplyContext, error) {
	m, err := p.svc.Users.Messages.Get("me", id.Native).Format("metadata").
		MetadataHeaders("Message-ID", "References", "Subject").Context(ctx).Do()
	if err != nil {
		return nil, p.wrap("get_reply_context", err)
	}
	out := &rfc5322.ReplyContext{ThreadID: m.ThreadId}
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "message-id":
				out.MessageID = h.Value
			case "references":
				out.References = h.Value
			}
		}
	}
	return out, nil
}

func natives(ids []mmail.ScopedID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.Native
	}
	return out
}

func labelStrings(ids []mmail.LabelID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

var _ interface {
	mmail.MessageWriter
	mmail.DraftManager
	mmail.LabelManager
	mmail.Destroyer
} = (*Provider)(nil)

// EffectOfApplying classifies a Gmail label id.
//
// Gmail's system labels are their own ids — TRASH, SPAM, INBOX — while a label somebody made
// is Label_<n> whatever they chose to call it. So a user label named "Trash" cannot be
// mistaken for the bin, and the bin cannot be renamed out of recognition either.
//
// Applying TRASH through BatchModify is the same act as Users.Messages.Trash above: the
// message leaves the inbox for the bin, and Gmail empties that on its own schedule.
// DeletingDestroysMail is false for every Gmail label, including TRASH and SPAM.
//
// A Gmail label is a tag, not a container: Users.Labels.Delete removes the label and the
// messages it was on stay where they are, minus that one tag. Gmail is the only provider here
// for which that is true, which is very likely why deleting a label was treated as ordinary
// everywhere.
func (p *Provider) DeletingDestroysMail(_ context.Context, _ mmail.LabelID) (bool, error) {
	return false, nil
}

func (p *Provider) EffectOfApplying(_ context.Context, id mmail.LabelID) (mmail.LabelEffect, error) {
	switch strings.ToUpper(string(id)) {
	case "TRASH":
		return mmail.EffectTrash, nil
	case "SPAM":
		return mmail.EffectSpam, nil
	}
	return mmail.EffectFile, nil
}
