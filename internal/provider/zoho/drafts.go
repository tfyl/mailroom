package zoho

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// --- DraftManager ---
//
// Zoho's public Mail API covers three of the five operations on this interface, and the two
// it does not cover are refused here by name rather than worked around.
//
// Saving is POST /accounts/{id}/messages with mode=draft, listing is the ordinary folder
// listing pointed at Drafts, and discarding is the ordinary message delete. Editing a saved
// draft and sending one are absent: Zoho's own index of message operations
// (https://www.zoho.com/mail/help/api/email-api.html) runs to twenty-five calls — send,
// reply, save draft, the listings, the reads, eleven modes of updatemessage, and delete —
// and none of them rewrites a stored message or puts one on the wire.
//
// Both gaps have an obvious workaround and both workarounds are refused, because each one
// changes what the caller asked for without being able to say so. The reasoning is at each
// method; it is the same reasoning that had this provider refuse to send with attachments
// rather than send without them, until there was an upload path to send them through.

func (p *Provider) CreateDraft(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	if !out.InReplyTo.Zero() {
		// Measured against the live mailbox: the save-draft endpoint refuses the two fields
		// that make a reply a reply. Posting mode=draft alongside action and
		// referencedMessageId — the pair Send uses, and the only threading Zoho's message API
		// offers — comes back
		//
		//	404 {"data":{"errorCode":"EXTRA_KEY_FOUND_IN_JSON"},"status":{"code":404,…}}
		//
		// which this provider maps to not_found, so the caller was told its message did not
		// exist. Nothing was wrong with the message.
		//
		// Saving it as an ordinary draft is the obvious workaround and is refused for the
		// usual reason: the draft would look right, sit in Drafts detached from the
		// conversation it answers, and nothing would have said so. Zoho documents inReplyTo
		// and refHeader as the alternative, and both take an RFC 5322 Message-ID that
		// mailroom does not hold — its ids are Zoho's own — so implementing this needs the
		// header fetched first, not a different field name.
		return mmail.ScopedID{}, &mmail.UnsupportedError{
			Provider: mmail.ProviderZoho, Account: p.account.Alias,
			Address: p.account.Address, Capability: mmail.CapDraft,
			Op: "saving a draft that replies to a message",
			Reason: "Zoho's save-draft endpoint refuses the fields that record which message a " +
				"reply answers, and saving it as an ordinary draft would leave it detached " +
				"from the conversation without saying so; send the reply directly, or write " +
				"the draft in Zoho's own client",
		}
	}

	body := p.composeBody(out)
	// mode is the entire difference between saving this and sending it, on an endpoint that
	// is otherwise the one Send posts to. Zoho documents it as required, taking draft or
	// template — https://www.zoho.com/mail/help/api/post-save-draft-template.html — so a
	// request that lost this field would not fail, it would deliver the mail.
	body["mode"] = "draft"

	// Attachments go up before the draft is stored, the same way Send does it, so a draft is
	// never saved without a file it was meant to carry.
	//
	// Unverified, and the least verified thing in this provider. Zoho's save-draft page
	// documents no attachment parameter at all — the attachments array is documented on the
	// send page, which posts to this same route — and this endpoint has already been observed
	// refusing a key it accepts on the send path with EXTRA_KEY_FOUND_IN_JSON. If it refuses
	// this one too the draft is not saved and the caller is told, which is the direction this
	// has to fail in; what must not happen is a draft stored with its files quietly dropped.
	if err := p.attachUploads(ctx, out, body); err != nil {
		return mmail.ScopedID{}, err
	}

	var created struct {
		MessageID flexString `json:"messageId"`
		FolderID  flexString `json:"folderId"`
	}
	if err := p.do(ctx, http.MethodPost, "/accounts/"+p.accountID+"/messages", nil, body, &created); err != nil {
		return mmail.ScopedID{}, err
	}

	// Zoho publishes no response body for this endpoint — the save-draft page shows the
	// request and stops — so the shape assumed here is the send's, which was observed against
	// a live mailbox to answer a messageId and no folderId. Both halves of that assumption
	// are handled rather than relied on: a folderId is used if one arrives, and an answer
	// with no messageId is refused.
	//
	// Refused rather than patched over, because the id is the whole product of this call. A
	// draft returned as "<drafts>/" would be rejected by splitNative at whatever the caller
	// did with it next, naming an id the caller never chose, for a draft that saved perfectly
	// well.
	if created.MessageID == "" {
		return mmail.ScopedID{}, fmt.Errorf("the draft may have been saved, but Zoho reported no " +
			"message id for it, so nothing can address it afterwards")
	}

	folder := created.FolderID.String()
	if folder == "" {
		// A draft is in Drafts, so the folder is looked up rather than invented — the same
		// repair Send needed when Zoho answered a send with no folder. If the lookup fails
		// there is no honest id to hand back.
		found, err := p.systemFolderID(ctx, folderDrafts)
		if err != nil {
			return mmail.ScopedID{}, fmt.Errorf("the draft was saved, but its id could not be "+
				"resolved: Zoho reported no folder for it and the Drafts folder could not be "+
				"read: %w", err)
		}
		folder = found
	}
	return p.scoped(folder, created.MessageID.String()), nil
}

// UpdateDraft is refused: Zoho publishes no way to edit a draft it has already stored.
//
// The one PUT in Zoho's message API is updatemessage, and its eleven modes all file a message
// — read, unread, move, flag, label, archive, spam — rather than change a word of it. Nothing
// else in the index touches a stored message's content.
//
// The workaround is to save a second draft and delete the first, and it is refused rather
// than done because this method reports no id. The caller would go on holding one that now
// addresses a deleted message, while the text it asked for sat in Drafts under an id nobody
// has — an edit that reports success and cannot be found afterwards. A refusal it can read is
// better, and the caller still has both halves: save the new text as a draft, and discard the
// old one.
func (p *Provider) UpdateDraft(_ context.Context, _ mmail.ScopedID, _ mmail.Outgoing) error {
	return &mmail.UnsupportedError{
		Provider: mmail.ProviderZoho, Account: p.account.Alias,
		Address: p.account.Address, Capability: mmail.CapDraft,
		Op: "editing a saved draft",
		Reason: "Zoho's mail API can save a draft but has no call that rewrites one; save the " +
			"new text as a second draft and discard the first, or edit it in Zoho's own client",
	}
}

// SendDraft is refused: Zoho publishes no way to send a draft it has already stored.
//
// Mail360 does — POST /accounts/{key}/drafts/{draftId} — and anyone looking for this will
// find it, so it is worth saying that it is a different product on a different host, with its
// own account key and its own MailApps scopes. mail.zoho.com has no /drafts route at all.
//
// The workaround is to read the draft back and post it as a fresh send, and that one is
// refused for a harder reason than the update above: it cannot be done faithfully. Zoho's
// metadata endpoint reports toAddress and ccAddress and carries no bcc field of any spelling
// (https://www.zoho.com/mail/help/api/get-email-meta-data.html), so a draft written to a
// blind copy would go out to fewer people than it was addressed to, with nothing in the
// result saying so — and its attachments would be dropped the same way. Quietly changing who
// receives a message is the one failure this provider must not have, and a send is the one
// operation that cannot be taken back once it has it.
func (p *Provider) SendDraft(_ context.Context, _ mmail.ScopedID) (mmail.ScopedID, error) {
	return mmail.ScopedID{}, &mmail.UnsupportedError{
		Provider: mmail.ProviderZoho, Account: p.account.Alias,
		Address: p.account.Address, Capability: mmail.CapSend,
		Op: "sending a draft Zoho has already saved",
		Reason: "Zoho's mail API has no send-this-draft call, and rebuilding the draft as a " +
			"fresh send would drop its blind copies and attachments without saying so; send " +
			"the message in one call instead of saving it first, or send the saved draft from " +
			"Zoho's own client",
	}
}

// DeleteDraft discards a draft with the ordinary message delete, which is the only delete
// Zoho has: DELETE /accounts/{a}/folders/{f}/messages/{m}, with an optional expunge flag
// (https://www.zoho.com/mail/help/api/delete-email.html).
//
// expunge is deliberately not sent. It defaults to false, which moves the message to Trash
// instead of destroying it, and that is what discarding a draft should mean: the draft leaves
// Drafts and stops appearing in ListDrafts, and somebody who discarded the wrong one can
// still get the words back. Destroying mail outright is what the destructive capability
// gates, and Delete is the method that spends it — sending expunge=true here would be that
// capability arriving through the discard door, on a grant that was never given it.
func (p *Provider) DeleteDraft(ctx context.Context, id mmail.ScopedID) error {
	folderID, messageID, err := splitNative(id.Native)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/accounts/%s/folders/%s/messages/%s", p.accountID, folderID, messageID)

	// Decoded into something rather than discarded, because do only inspects the envelope
	// when it has somewhere to put the data — and a Zoho endpoint can answer HTTP 200 with a
	// failing envelope inside it. A delete that failed in the envelope and was read as a
	// success would leave the draft in the mailbox and report it gone. cId is the field
	// Zoho's documented delete response carries; nothing here uses the value.
	var deleted struct {
		CID flexString `json:"cId"`
	}
	return p.do(ctx, http.MethodDelete, path, nil, nil, &deleted)
}

// ListDrafts pages the Drafts folder through the ordinary listing endpoint, which takes a
// folderId — Zoho has no drafts-specific listing.
//
// It pages by offset like Search does, and inherits the same warning: Zoho's paging is not
// stable, so a walk of a large Drafts folder can return one draft twice and step over
// another. The quirk is declared once for the provider rather than restated per method.
func (p *Provider) ListDrafts(ctx context.Context, cursor string) (mmail.Page[mmail.Message], error) {
	start, err := offsetCursor(cursor)
	if err != nil {
		return mmail.Page[mmail.Message]{}, err
	}
	folder, err := p.systemFolderID(ctx, folderDrafts)
	if err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	query := url.Values{}
	query.Set("folderId", folder)
	query.Set("start", strconv.Itoa(start))
	query.Set("limit", strconv.Itoa(defaultPageSize))
	query.Set("includeto", "true")

	var raw []message
	if err := p.get(ctx, "/accounts/"+p.accountID+"/messages/view", query, &raw); err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	items := make([]mmail.Message, 0, len(raw))
	for _, m := range raw {
		// The listing is documented to report folderId and the live mailbox does, but every
		// id in this provider needs one, and a draft that arrived without it would become
		// "/1234" — malformed at whatever the caller did next rather than here. These
		// messages came out of the folder that was asked for, so that is the folder they are
		// in; this is the id the caller was already given, not a guess at a new one.
		if m.FolderID == "" {
			m.FolderID = flexString(folder)
		}
		converted := p.convert(m)
		// Zoho reports no draft flag on a listing; what makes these drafts is the folder they
		// were read from. Deriving it here is the difference between a caller seeing a draft
		// and seeing an ordinary message it might try to reply to.
		converted.Flags.Draft = true
		items = append(items, converted)
	}

	page := mmail.Page[mmail.Message]{Items: items}
	// A full page implies there may be more. Zoho reports no total, so a short page is the
	// only honest end-of-list signal there is.
	if len(raw) == defaultPageSize {
		page.Cursor = strconv.Itoa(start + defaultPageSize)
	}
	return page, nil
}
