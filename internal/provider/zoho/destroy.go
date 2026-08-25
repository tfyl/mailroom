package zoho

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- Destroyer ---
//
// Zoho reaches all three operations through two calls it already publishes. The move mode of
// updatemessage (https://www.zoho.com/mail/help/api/move-email.html) puts a message in the bin
// and takes it back out again, and the ordinary message delete
// (https://www.zoho.com/mail/help/api/delete-email.html) destroys one when it is given
// expunge=true. Both are covered by ZohoMail.messages.ALL, which this provider already asks
// for, so no linked mailbox has to be consented again for the capability to start working.
//
// Before this, `destructive` was not claimed here at all and moving a message into the Trash
// folder through ApplyLabels was the only way to bin Zoho mail. That route still exists and is
// still gated — EffectOfApplying classifies the Trash and Spam folders as destructive — so
// what follows is a second, named way in rather than a way around.
//
// # What a move does to an id the caller is still holding
//
// A Zoho id here is <folder>/<message>, and a move changes the folder half of it without
// telling anyone. The id a caller passed to Trash goes on naming the folder the message was in
// beforehand, so reading it, listing its attachments or fetching its content afterwards
// addresses a message that is no longer at that path — which Zoho answers as 400 "messageId is
// invalid" and this provider maps to not_found. A caller that trashes a message and then wants
// to touch it again has to find it again: the id it holds is spent. Nothing in the interface
// can say so, because Trash reports an error and nothing else, which is why it is said here.
//
// Two consequences follow and they pull in opposite directions:
//
//   - Trash and Untrash survive a spent id, because updatemessage addresses messages by id
//     alone and takes a source folder only as an optional hint. That is why Untrash does not
//     check that what it is restoring was in the bin: after a Trash the folder half of the
//     caller's id still says Inbox, so a check against it would refuse precisely the round
//     trip it would exist to protect.
//   - Delete does not survive one, because its folder travels in the path. Destroying a
//     message that was trashed, using an id from before the trashing, addresses nothing and
//     fails as not_found. That is the safe direction to fail in, but it means a caller has to
//     locate the message in Trash before it can destroy it.
//
// Unverified: whether Zoho's numeric message id itself survives a move between folders. The
// move endpoint's documentation says nothing either way and none of this has been run against
// a mailbox. If the id changes as well as the folder, the second point is worse than described
// rather than differently shaped, and re-finding the message is still the answer.

// The two system folders trashing moves mail between. Zoho's folder listing documents the
// system set as Inbox, Drafts, Templates, Snoozed, Sent, Spam, Trash and Outbox —
// https://www.zoho.com/mail/help/api/get-all-folder-details.html.
const (
	folderInbox = "Inbox"
	folderTrash = "Trash"
)

// Trash moves messages into the bin, where they stay recoverable until Zoho empties it.
//
// A move rather than the delete endpoint with expunge left off, which Zoho documents as having
// the same effect. The move is the call with an inverse: Untrash is the same request with a
// different destination, so binning and restoring cannot drift apart as two endpoints would.
// It also carries every id in one request, where the delete takes one message per call.
func (p *Provider) Trash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.moveToSystemFolder(ctx, ids, folderTrash)
}

// Untrash brings messages back to the inbox.
//
// Not back to where they came from: Zoho records that a message is in Trash and not what
// folder it was in before, so there is nothing to restore to. The inbox is the honest guess and
// the same one Microsoft's Untrash makes here. Somebody whose mail was filed in a project
// folder gets it back in the inbox, which is worth knowing before it looks like a bug.
func (p *Provider) Untrash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.moveToSystemFolder(ctx, ids, folderInbox)
}

func (p *Provider) moveToSystemFolder(ctx context.Context, ids []mmail.ScopedID, name string) error {
	if len(ids) == 0 {
		return nil
	}
	// Parsed before the folder is resolved, so a batch this provider cannot read costs no
	// request at all and fails naming the id rather than after a folder listing that went
	// nowhere.
	messageIDs, err := nativeMessageIDs(ids)
	if err != nil {
		return err
	}
	destination, err := p.systemFolderID(ctx, name)
	if err != nil {
		return err
	}

	// destfolderId carries the digits as a JSON string, which is what ApplyLabels' move
	// already sends, though Zoho documents the field as a long. Neither has been run against a
	// mailbox. Zoho spells its own ids both ways on adjacent endpoints — see flexString — and
	// answers a genuinely wrong type with DATATYPE_NOT_MATCHED rather than silence, so a
	// mismatch here would surface as a failed move rather than as mail that quietly stayed
	// put; and keeping both moves spelt the same way means one fix rather than two.
	body := map[string]any{
		"mode":         "moveMessage",
		"destfolderId": destination,
		"messageId":    messageIDs,
	}

	// Not p.updateMessages, which passes nil for out — and do reads the envelope only when it
	// is given somewhere to decode into, so a move that failed inside an HTTP 200 would be
	// reported as done. That matters more here than on a label change: a Trash reported
	// successful when the message never moved leaves mail sitting where somebody has decided
	// it should not be, and they have been told otherwise.
	//
	// Decoded into a raw message rather than a struct because Zoho's documented answer to a
	// move is an envelope with no data object at all. Naming a field would be inventing one;
	// nothing here wants the value, only the status beside it.
	var moved json.RawMessage
	return p.do(ctx, http.MethodPut, "/accounts/"+p.accountID+"/updatemessage", nil, body, &moved)
}

// Delete destroys mail outright. There is no bin behind it and no undo, which is why it is
// reached through the destructive capability rather than through labels.
//
// expunge=true is the whole of the difference from DeleteDraft, which deliberately leaves the
// flag off so that a discarded draft lands in Trash and can be recovered. Zoho documents it as
// a query parameter defaulting to false, with true deleting the message "permanently without
// moving it to the trash folder" — https://www.zoho.com/mail/help/api/delete-email.html. A
// Delete that quietly went to the bin instead would tell a caller something irreversible had
// happened when it had not, and leave whoever wanted the mail gone believing that it was.
//
// Zoho has no bulk delete: its published index of message operations lists one delete, and it
// addresses one message through the path. So this is a request per message, and a failure part
// way through has already destroyed the messages before it, permanently, with nothing able to
// put them back. That is why the whole batch is parsed before the first request goes out — a
// batch carrying one id this provider cannot read destroys nothing at all, rather than
// destroying its way up to the bad one and then reporting a parse error.
func (p *Provider) Delete(ctx context.Context, ids []mmail.ScopedID) error {
	targets, err := splitNatives(ids)
	if err != nil {
		return err
	}

	// A query parameter, not a body field; Zoho's delete carries no body.
	query := url.Values{"expunge": []string{"true"}}

	for _, target := range targets {
		path := fmt.Sprintf("/accounts/%s/folders/%s/messages/%s",
			p.accountID, target.folder, target.message)

		// Decoded for the reason DeleteDraft decodes: do inspects the envelope only when it
		// has somewhere to put the data, and a delete that failed under an HTTP 200 would
		// otherwise be reported as a message destroyed. On this method that is the worse
		// direction to be wrong in — a caller told the mail is gone stops looking for it, and
		// then nobody notices that it is still in the mailbox somebody wanted it out of. cId
		// is the field Zoho's documented delete response carries; nothing here uses the value.
		var deleted struct {
			CID flexString `json:"cId"`
		}
		if err := p.do(ctx, http.MethodDelete, path, query, nil, &deleted); err != nil {
			return err
		}
	}
	return nil
}

// messageRef is a Zoho id taken apart. The move endpoint needs only the message half, which is
// what nativeMessageIDs hands back; the delete endpoint puts both halves in the path.
type messageRef struct{ folder, message string }

func splitNatives(ids []mmail.ScopedID) ([]messageRef, error) {
	out := make([]messageRef, 0, len(ids))
	for _, id := range ids {
		folder, message, err := splitNative(id.Native)
		if err != nil {
			return nil, err
		}
		out = append(out, messageRef{folder: folder, message: message})
	}
	return out, nil
}

var _ mmail.Destroyer = (*Provider)(nil)
