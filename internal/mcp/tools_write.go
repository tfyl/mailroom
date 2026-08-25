package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/aggregate"
	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
)

// SendCounter reports how many sends a grant has made inside a window, backing the per-grant
// send limit. Counting from the audit log means the limit survives a restart.
type SendCounter interface {
	CountSends(ctx context.Context, id grant.ID, since time.Time) (int, error)
}

type addressArg struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

func toAddresses(in []addressArg) []mail.Address {
	out := make([]mail.Address, len(in))
	for i, a := range in {
		out[i] = mail.Address{Name: a.Name, Email: a.Email}
	}
	return out
}

// Attachment size limits.
//
// Two different numbers because the two sources are bounded by different things. Inline
// content travels inside the MCP request, which the transport caps at 4 MiB, so an inline
// attachment has to leave room for the rest of the call — the body text, the recipients, the
// JSON-RPC envelope. Content that never enters the request — a mailbox reference or an
// upload — is bounded only by what the provider will accept, which is why the total is
// mail.MaxAttachmentBytes and is stated there.
//
// One consequence worth stating, because it looks like a bug from outside. The transport
// rejects a request body over 4 MiB before any of this code runs, so an inline attachment
// much above 2 MiB dies as an opaque 413 rather than reaching the explanation below. The
// explanation still earns its place for the band between the two, and the schema tells a
// caller the cap up front — which is the only thing that helps before the request is built.
// The total ceiling is reachable only by reference or by upload, which is the direction
// callers should be going anyway.
const (
	maxInlineAttachment = 2 << 20 // 2 MiB raw, ~2.7 MiB once base64'd
	// The per-message total is mail.MaxAttachmentBytes so that this path and the upload
	// route cannot disagree about what a mail server will take.
	maxTotalAttachments = mail.MaxAttachmentBytes
)

// attachmentInput is one attachment to add to an outgoing message, from one of three places.
//
// Two of the three keep the bytes out of the conversation entirely, and those are the ones to
// reach for. `from_message` reuses something already in a mailbox — "forward me that invoice",
// the common request — and the bytes are read from one mailbox and written to another
// server-side. `blob_id` names something uploaded to a signed URL by the client itself, which
// is the only way a file on the client's own disk can ever get here: MCP gives this server no
// access to that filesystem. Neither is bounded by the transport's request limit.
//
// Inline base64 remains for genuinely new, genuinely small content, and is capped hard.
type attachmentInput struct {
	// Reference form.
	FromMessage  string `json:"from_message,omitempty" jsonschema:"Message id holding the attachment to reuse. Reading it needs the attachments capability on that mailbox, which is checked separately from the mailbox being composed in."`
	AttachmentID string `json:"attachment_id,omitempty" jsonschema:"Which attachment on that message, from its attachment manifest."`

	// Uploaded form: bytes the client PUT to a URL from mail_upload_url.
	BlobID string `json:"blob_id,omitempty" jsonschema:"An upload from mail_upload_url. Use this for a file you hold locally or anything over about 2 MiB: you PUT the bytes to the signed URL yourself, then name the blob_id here."`

	// Inline form.
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"Base64 file content, for something newly composed rather than forwarded. Capped at 2 MiB of raw bytes, and the whole request is capped below 4 MiB by the transport, so anything larger must come from a mailbox with from_message rather than inline."`

	// Applies to both: required inline, optional override for a reference.
	Filename string `json:"filename,omitempty" jsonschema:"Name the recipient sees. Required for inline content; defaults to the original name when reusing an attachment."`
	MimeType string `json:"mime_type,omitempty" jsonschema:"Defaults to application/octet-stream inline, or the original type when reusing an attachment."`
}

func (a attachmentInput) byReference() bool { return a.FromMessage != "" || a.AttachmentID != "" }

// sources counts the places this attachment claims to come from. Exactly one is required, and
// counting rather than checking pairwise is what keeps that true as a third was added.
func (a attachmentInput) sources() int {
	n := 0
	for _, present := range []bool{a.byReference(), a.ContentBase64 != "", a.BlobID != ""} {
		if present {
			n++
		}
	}
	return n
}

type composeArgs struct {
	Account     string            `json:"account,omitempty" jsonschema:"Which mailbox to compose from. Not needed when replying: the mailbox comes from in_reply_to."`
	InReplyTo   string            `json:"in_reply_to,omitempty" jsonschema:"Message id being replied to. Sets threading headers so the reply lands in the same conversation."`
	To          []addressArg      `json:"to,omitempty"`
	Cc          []addressArg      `json:"cc,omitempty"`
	Bcc         []addressArg      `json:"bcc,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Body        string            `json:"body,omitempty" jsonschema:"Plain text body"`
	HTML        string            `json:"html,omitempty" jsonschema:"Optional HTML alternative"`
	Attachments []attachmentInput `json:"attachments,omitempty" jsonschema:"Files to attach. Reuse one already in a mailbox with from_message and attachment_id, name an upload with blob_id, or supply small new content inline as base64."`
}

// resolveComposeTarget works out which mailbox a compose call is for.
//
// A reply takes its account from the message being replied to, which is both less for the
// caller to get right and impossible to get wrong: a reply cannot accidentally be sent from
// a different mailbox than the conversation it belongs to.
func (t *Tools) resolveComposeTarget(ctx context.Context, g *grant.Grant, tool string, args composeArgs, c mail.Capability) (mail.Account, mail.ScopedID, error) {
	if args.InReplyTo != "" {
		id, err := mail.ParseScopedID(args.InReplyTo)
		if err != nil {
			return mail.Account{}, mail.ScopedID{}, err
		}
		acct, err := t.gate.ResolveOne(ctx, g, tool, id, c)
		return acct, id, err
	}

	var selector []string
	if args.Account != "" {
		selector = []string{args.Account}
	}
	accounts, err := t.gate.Resolve(ctx, g, tool, selector, c)
	if err != nil {
		return mail.Account{}, mail.ScopedID{}, err
	}
	if len(accounts) > 1 {
		// Never guess which mailbox to send from. Picking one silently is the kind of
		// mistake a person only notices after the message has gone.
		//
		// Bare aliases, not alias-and-address: this list exists to be chosen from, and
		// whatever it offers is what a caller will put back in `account`. Naming the
		// mailboxes more fully here would cost a round trip to a selector that resolves
		// nothing. mail_accounts is where the addresses are.
		names := make([]string, len(accounts))
		for i, a := range accounts {
			names[i] = a.Alias
		}
		return mail.Account{}, mail.ScopedID{}, fmt.Errorf(
			"this grant covers several mailboxes (%v); name one with `account`", names)
	}
	return accounts[0], mail.ScopedID{}, nil
}

// --- mail_draft ---

type draftArgs struct {
	composeArgs
	Action  string `json:"action,omitempty" jsonschema:"create, update or delete. Defaults to create."`
	DraftID string `json:"draft_id,omitempty" jsonschema:"Required to update or delete a draft."`
}

func (t *Tools) handleDraft(ctx context.Context, _ *mcp.CallToolRequest, args draftArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if args.Action == "" {
		args.Action = "create"
	}

	// Updating and deleting name an existing draft, so the account comes from its id.
	if args.Action == "update" || args.Action == "delete" {
		id, err := mail.ParseScopedID(args.DraftID)
		if err != nil {
			return t.invalid(ctx, g, grant.Audit{
				Tool: "mail.draft", Capability: mail.CapDraft,
				Detail: grant.Detail{Action: args.Action},
			}, fmt.Errorf("draft_id is required to %s a draft: %w", args.Action, err)), nil, nil
		}
		// The two actions are authorized separately even though they share a path. Removing a
		// draft is not an edit of it: the text is gone and the operator may have written it,
		// so it answers to `discard` while rewriting the same draft answers to `draft`.
		needs := mail.CapDraft
		if args.Action == "delete" {
			needs = mail.CapDiscard
		}
		acct, err := t.gate.ResolveOne(ctx, g, "mail.draft", id, needs)
		if err != nil {
			return toolError(err), nil, nil
		}
		drafts, err := t.draftManager(ctx, acct)
		if err != nil {
			return toolError(err), nil, nil
		}

		if args.Action == "delete" {
			err = drafts.DeleteDraft(ctx, id)
			note := t.auditChange(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.draft", Capability: needs,
				Affected: counted(1),
				Detail:   grant.Detail{Action: "delete", IDs: []string{id.String()}},
			}, err)
			if err != nil {
				return toolError(err), nil, nil
			}
			return result(noted(map[string]any{"deleted": args.DraftID}, note))
		}

		attachments, err := t.resolveAttachments(ctx, g, args.Attachments)
		if err != nil {
			return toolError(err), nil, nil
		}

		// An update rebuilds the whole message, threading headers included, so a reply being
		// revised has to say again what it answers or it leaves the conversation it belongs
		// to. The account still comes from the draft; this only decides where it threads.
		var replyTo mail.ScopedID
		if args.InReplyTo != "" {
			replyTo, err = mail.ParseScopedID(args.InReplyTo)
			if err != nil {
				return t.invalid(ctx, g, draftEntry(acct.ID, "update", args.composeArgs),
					fmt.Errorf("in_reply_to: %w", err)), nil, nil
			}
			if replyTo.Account != id.Account {
				return t.invalid(ctx, g, draftEntry(acct.ID, "update", args.composeArgs), fmt.Errorf(
					"in_reply_to names a message in a different mailbox than the draft")), nil, nil
			}
		}

		err = drafts.UpdateDraft(ctx, id, t.outgoing(acct, args.composeArgs, replyTo, attachments))
		entry := draftEntry(acct.ID, "update", args.composeArgs)
		entry.Detail.IDs = []string{id.String()}
		note := t.auditChange(ctx, g, entry, err)
		if err != nil {
			return toolError(err), nil, nil
		}
		return result(noted(map[string]any{"updated": args.DraftID}, note))
	}

	acct, replyTo, err := t.resolveComposeTarget(ctx, g, "mail.draft", args.composeArgs, mail.CapDraft)
	if err != nil {
		return toolError(err), nil, nil
	}
	drafts, err := t.draftManager(ctx, acct)
	if err != nil {
		return toolError(err), nil, nil
	}

	attachments, err := t.resolveAttachments(ctx, g, args.Attachments)
	if err != nil {
		return toolError(err), nil, nil
	}

	id, err := drafts.CreateDraft(ctx, t.outgoing(acct, args.composeArgs, replyTo, attachments))
	entry := draftEntry(acct.ID, "create", args.composeArgs)
	if err == nil {
		entry.Detail.IDs = []string{id.String()}
	}
	auditNote := t.auditChange(ctx, g, entry, err)
	if err != nil {
		return toolError(err), nil, nil
	}
	return result(noted(map[string]any{
		"draft_id":        id.String(),
		"account":         acct.Alias,
		"account_address": acct.Address,
		"note":            "Saved as a draft. Sending it needs the `send` capability.",
	}, auditNote))
}

// --- mail_send ---

type sendArgs struct {
	composeArgs
	DraftID string `json:"draft_id,omitempty" jsonschema:"Send an existing draft instead of composing a new message."`
}

func (t *Tools) handleSend(ctx context.Context, _ *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Sending an existing draft is still sending, and takes the send capability rather than
	// draft. Routing it through mail_draft would have let a draft-only grant send.
	//
	// Every provider removes the draft as part of delivering it — that is what sending a
	// draft means — and that removal is deliberately not `discard`. `discard` is for throwing
	// unsent text away; a draft that became a message was not thrown away. Requiring it here
	// would break `send` for every grant that holds it without `discard`, which after this
	// split is every grant that existed before it.
	if args.DraftID != "" {
		id, err := mail.ParseScopedID(args.DraftID)
		if err != nil {
			return t.invalid(ctx, g, grant.Audit{
				Tool: "mail.send", Capability: mail.CapSend,
				Detail: grant.Detail{Action: "draft"},
			}, err), nil, nil
		}
		acct, err := t.gate.ResolveOne(ctx, g, "mail.send", id, mail.CapSend)
		if err != nil {
			return toolError(err), nil, nil
		}
		if err := t.checkSendLimit(ctx, g); err != nil {
			return t.invalid(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.send", Capability: mail.CapSend,
				Detail: grant.Detail{Action: "draft", IDs: []string{id.String()}},
			}, err), nil, nil
		}
		drafts, err := t.draftManager(ctx, acct)
		if err != nil {
			return toolError(err), nil, nil
		}
		// Sending a draft names the draft and no recipients, because this call was given
		// none: they were set when the draft was written, and inventing them here would mean
		// a second provider round trip on the one path where the message has already gone.
		// The page says which it was rather than leaving the recipients looking absent.
		//
		// Held after the mailbox can be shown to support drafts, so approving cannot fail on
		// a provider that was never going to do this. The queued row names the draft rather
		// than copying it: the draft is already in the mailbox, where its owner can read it
		// and change it before they approve — the one held action whose subject is editable
		// while it waits.
		if g.Mode.Holds() {
			return t.heldResult(ctx, g, acct, grant.Audit{
				AccountID: acct.ID, Tool: "mail.send", Capability: mail.CapSend,
				Detail: grant.Detail{Action: "draft", IDs: []string{id.String()}},
			}, held.KindSendDraft,
				"send the saved draft "+id.String(), held.SendPayload{Draft: id.String()})
		}
		sent, err := drafts.SendDraft(ctx, id)
		note := t.auditChange(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.send", Capability: mail.CapSend,
			Detail: grant.Detail{Action: "draft", IDs: []string{id.String()}},
		}, err)
		if err != nil {
			return toolError(err), nil, nil
		}
		return result(noted(map[string]any{
			"sent": sent.String(), "account": acct.Alias, "account_address": acct.Address,
		}, note))
	}

	if len(args.To) == 0 {
		return t.invalid(ctx, g, grant.Audit{Tool: "mail.send", Capability: mail.CapSend},
			fmt.Errorf("at least one recipient is required")), nil, nil
	}

	acct, replyTo, err := t.resolveComposeTarget(ctx, g, "mail.send", args.composeArgs, mail.CapSend)
	if err != nil {
		return toolError(err), nil, nil
	}
	if err := t.checkSendLimit(ctx, g); err != nil {
		// A refused send is the one refusal an operator is most likely to come looking for,
		// and it is worth as much detail as one that went through: the limit exists because
		// the alternative is a message reaching every contact the mailbox has.
		return t.invalid(ctx, g, sendEntry(acct.ID, args.composeArgs), err), nil, nil
	}

	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return toolError(err), nil, nil
	}
	writer, ok := p.(mail.MessageWriter)
	if !ok {
		return toolError(&mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address, Capability: mail.CapSend,
		}), nil, nil
	}

	// Resolved after the send limit is checked but before the send itself: fetching a large
	// attachment only to be refused by the rate limiter wastes a provider round trip, and
	// composing a message whose attachments failed to resolve would send it without them.
	attachments, err := t.resolveAttachments(ctx, g, args.Attachments)
	if err != nil {
		return toolError(err), nil, nil
	}

	outgoing := t.outgoing(acct, args.composeArgs, replyTo, attachments)
	// Held with the message already assembled: the recipients resolved, the attachment bytes
	// gathered. An approval weeks later then sends exactly what the client composed, rather
	// than re-resolving an upload URL that expired or a source message that was deleted in
	// the meantime. The audit row is the one this send would have written, so the recipients
	// and the subject are on the record at the moment the call was made rather than only when
	// somebody gets round to answering it.
	if g.Mode.Holds() {
		return t.heldResult(ctx, g, acct, sendEntry(acct.ID, args.composeArgs), held.KindSend,
			held.DescribeSend(outgoing), held.SendPayload{Outgoing: outgoing})
	}

	sent, err := writer.Send(ctx, outgoing)
	entry := sendEntry(acct.ID, args.composeArgs)
	if err == nil {
		entry.Detail.IDs = []string{sent.String()}
	}
	note := t.auditChange(ctx, g, entry, err)
	if err != nil {
		return toolError(err), nil, nil
	}
	// A send is the one result nobody can undo after reading it, so it names the mailbox it
	// left from in full: the alias the caller chose it by, and the address the recipient will
	// see it came from.
	return result(noted(map[string]any{
		"sent": sent.String(), "account": acct.Alias, "account_address": acct.Address,
	}, note))
}

// checkSendLimit caps outbound volume per grant. A compromised or confused agent that can
// send twenty messages is a bad afternoon; one that can send two thousand is an incident
// with every contact the mailbox has.
func (t *Tools) checkSendLimit(ctx context.Context, g *grant.Grant) error {
	if t.sends == nil || t.sendLimit <= 0 {
		return nil
	}
	used, err := t.sends.CountSends(ctx, g.ID, time.Now().Add(-t.sendWindow))
	if err != nil {
		// Fail closed: if the count cannot be read the limit cannot be enforced, and an
		// unenforceable limit should stop the send rather than wave it through.
		return fmt.Errorf("could not verify the send limit, so the send was not attempted: %w", err)
	}
	if used >= t.sendLimit {
		return fmt.Errorf("send limit reached for this grant: %d in the last %s. "+
			"It resets as older sends age out", t.sendLimit, t.sendWindow)
	}
	return nil
}

// --- mail_modify ---

type modifyArgs struct {
	IDs          []string `json:"ids" jsonschema:"Message ids. May span mailboxes; each is authorized separately."`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
	Archive      bool     `json:"archive,omitempty" jsonschema:"Remove from the inbox"`
	Read         *bool    `json:"read,omitempty"`
	Starred      *bool    `json:"starred,omitempty"`
}

func (t *Tools) handleModify(ctx context.Context, _ *mcp.CallToolRequest, args modifyArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(args.IDs) == 0 {
		return t.invalid(ctx, g, grant.Audit{Tool: "mail.modify", Capability: mail.CapLabels},
			fmt.Errorf("at least one message id is required")), nil, nil
	}
	if args.Read == nil && args.Starred == nil && !args.Archive &&
		len(args.AddLabels) == 0 && len(args.RemoveLabels) == 0 {
		// A call that asks for no change would otherwise touch every mailbox it named, change
		// nothing, and report the count of messages it did not modify.
		return t.invalid(ctx, g, grant.Audit{
			Tool: "mail.modify", Capability: mail.CapLabels, Affected: counted(len(args.IDs)),
			Detail: grant.Detail{IDs: args.IDs},
		}, fmt.Errorf(
			"this asked for no change: set at least one of add_labels, remove_labels, "+
				"archive, read or starred")), nil, nil
	}

	outcomes := aggregate.NewOutcomes()
	grouped := t.groupByAccount(ctx, g, "mail.modify", args.IDs, mail.CapLabels, outcomes)

	// Read and starred are a flag update, not a label change. Gmail keeps both as labels
	// called UNREAD and STARRED, and expressing them that way sent every other provider a
	// label id from Gmail's namespace: Zoho and Microsoft refuse it as malformed, and IMAP —
	// which cannot express a label removal at all — used to return success having done
	// nothing, so "mark these read" and "archive these" reported as done on every IMAP
	// mailbox and changed none of them.
	update := mail.FlagUpdate{Read: args.Read, Starred: args.Starred}

	add := toLabelIDs(args.AddLabels)
	remove := toLabelIDs(args.RemoveLabels)
	if args.Archive {
		// Archiving stays Gmail vocabulary for dropping the inbox label, and stays a label
		// change, because that is what it is: providers with folders are not asked to invent
		// an "archive", they are asked for something they can refuse by name.
		remove = append(remove, "INBOX")
	}

	// Every mailbox is classified before any of them is touched.
	//
	// The capability is a property of the grant and the effect is a property of the provider,
	// so a batch spanning two mailboxes can be ordinary filing in one and a trashing in the
	// other. Refusing the whole call is the only honest answer to that: the caller asked for
	// one change, and performing it where it happened to be harmless would leave a client
	// reporting a filing job it half did, against mailboxes it cannot tell apart.
	type target struct {
		acct    mail.Account
		ids     []mail.ScopedID
		labels  mail.LabelManager
		applies []mail.DestructiveApply
	}
	var targets []target
	var destructive bool
	for acct, ids := range grouped {
		labels, err := t.labelManager(ctx, acct)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}
		// A classification that could not be made has not passed, so the mailbox fails rather
		// than being labelled on the assumption that nothing here is the bin.
		applies, err := mail.DestructiveApplies(ctx, labels, add)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}
		destructive = destructive || len(applies) > 0
		targets = append(targets, target{acct: acct, ids: ids, labels: labels, applies: applies})
	}

	if destructive && !g.Caps.Has(mail.CapDestructive) {
		for _, target := range targets {
			if len(target.applies) == 0 {
				continue
			}
			err := destructiveRefusal(g, target.acct, target.applies)
			// Recorded against `destructive`, which is what was missing, rather than against
			// the capability the tool is registered under. A row reading "mail.modify /
			// labels" is what let this sit unnoticed: the operator's page said a label was
			// changed, and nothing on it said mail had been binned.
			_ = t.gate.Record(ctx, g, grant.Audit{
				AccountID: target.acct.ID, Tool: "mail.modify", Capability: mail.CapDestructive,
				Affected: counted(len(target.ids)),
				Outcome:  grant.RefusedAs(err), Reason: err.Error(),
				Detail: grant.Detail{
					Action: describeModify(add, remove, update), IDs: scopedIDs(target.ids),
				},
			})
			return toolError(err), nil, nil
		}
	}

	for _, target := range targets {
		acct, ids, labels := target.acct, target.ids, target.labels

		// Held whole, rather than the destructive half being queued and the rest performed.
		// Splitting one call into a change that happened and a change that is waiting gives
		// the client something it cannot report accurately and the owner something they
		// cannot decide about.
		if len(target.applies) > 0 && g.Mode.Holds() {
			body, err := t.hold(ctx, g, acct, grant.Audit{
				AccountID: acct.ID, Tool: "mail.modify", Capability: mail.CapDestructive,
				Affected: counted(len(ids)),
				Detail: grant.Detail{
					Action: describeModify(add, remove, update), IDs: scopedIDs(ids),
				},
			}, held.KindModify,
				destructiveSummary(target.applies, len(ids), acct.Alias),
				held.ModifyPayload{
					IDs: scopedIDs(ids), Add: labelStrings(add), Remove: labelStrings(remove),
					Read: update.Read, Starred: update.Starred,
				})
			if err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			outcomes.OK(acct, body)
			continue
		}

		var err error

		// The flags go first. Either half can be refused by a provider, and doing the one
		// that works before the one that might not means a mailbox is left in the state the
		// caller asked for as far as it could be honoured, rather than in neither.
		//
		// Neither half is called with nothing to do: a provider asked to perform an empty
		// change is a request on somebody's mailbox that exists only to be ignored.
		if !update.Empty() {
			err = labels.SetFlags(ctx, ids, update)
		}
		if err == nil && (len(add) > 0 || len(remove) > 0) {
			err = labels.ApplyLabels(ctx, ids, add, remove)
		}
		// Recorded against whichever capability the call actually spent, so a binning does not
		// go on the record as a filing.
		capability := mail.CapLabels
		if len(target.applies) > 0 {
			capability = mail.CapDestructive
		}
		// What changed, not only that something did. A modify row that said only "ok" left
		// the operator to work out from the mailbox itself what an agent had done to it,
		// which is the one thing that may no longer be there to look at.
		note := t.auditChange(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.modify", Capability: capability,
			Affected: counted(len(ids)),
			Detail: grant.Detail{
				Action: describeModify(add, remove, update), IDs: scopedIDs(ids),
			},
		}, err)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}
		outcomes.OK(acct, noted(map[string]any{"modified": len(ids)}, note))
	}

	if outcomes.Failed() {
		return errorWithDetail("no message could be modified", outcomes.Payload()), nil, nil
	}
	return result(outcomes.Payload())
}

// --- mail_trash ---

type trashArgs struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action,omitempty" jsonschema:"trash, untrash or delete. Defaults to trash. delete cannot be undone."`
}

func (t *Tools) handleTrash(ctx context.Context, _ *mcp.CallToolRequest, args trashArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(args.IDs) == 0 {
		return t.invalid(ctx, g, grant.Audit{Tool: "mail.trash", Capability: mail.CapDestructive},
			fmt.Errorf("at least one message id is required")), nil, nil
	}
	if args.Action == "" {
		args.Action = "trash"
	}
	if args.Action != "trash" && args.Action != "untrash" && args.Action != "delete" {
		return t.invalid(ctx, g, grant.Audit{Tool: "mail.trash", Capability: mail.CapDestructive},
			fmt.Errorf("action must be trash, untrash or delete; got %q", args.Action)), nil, nil
	}

	outcomes := aggregate.NewOutcomes()
	grouped := t.groupByAccount(ctx, g, "mail.trash", args.IDs, mail.CapDestructive, outcomes)

	// Untrash is never held. Holding it would make putting mail back need permission while
	// the thing that took it away is what the permission is for.
	holding := g.Mode.Holds() && args.Action != "untrash"

	for acct, ids := range grouped {
		p, err := t.providers.For(ctx, acct)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}
		destroyer, ok := p.(mail.Destroyer)
		if !ok {
			outcomes.Fail(acct, &mail.UnsupportedError{
				Provider: p.ID(), Account: acct.Alias, Address: acct.Address,
				Capability: mail.CapDestructive})
			continue
		}

		if holding {
			// One queued action per mailbox, matching how the call is grouped and how its
			// result is reported. An operator approving "delete 4 messages in work" is
			// answering about one mailbox, which is the granularity they can decide at.
			body, err := t.hold(ctx, g, acct, grant.Audit{
				AccountID: acct.ID, Tool: "mail.trash", Capability: mail.CapDestructive,
				Affected: counted(len(ids)),
				Detail:   grant.Detail{Action: args.Action, IDs: scopedIDs(ids)},
			}, held.KindTrash,
				fmt.Sprintf("%s %d %s in %s", args.Action, len(ids),
					plural(len(ids), "message"), acct.Alias),
				held.TrashPayload{Action: args.Action, IDs: scopedIDs(ids)})
			if err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			outcomes.OK(acct, body)
			continue
		}

		switch args.Action {
		case "trash":
			err = destroyer.Trash(ctx, ids)
		case "untrash":
			err = destroyer.Untrash(ctx, ids)
		case "delete":
			err = destroyer.Delete(ctx, ids)
		}

		note := t.auditChange(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.trash", Capability: mail.CapDestructive,
			Affected: counted(len(ids)),
			Detail:   grant.Detail{Action: args.Action, IDs: scopedIDs(ids)},
		}, err)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}
		outcomes.OK(acct, noted(map[string]any{args.Action: len(ids)}, note))
	}

	if outcomes.Failed() {
		return errorWithDetail("the "+args.Action+" failed on every mailbox it named", outcomes.Payload()), nil, nil
	}
	return result(outcomes.Payload())
}

// --- mail_labels ---

type labelsArgs struct {
	Action  string `json:"action,omitempty" jsonschema:"list, create or delete. Defaults to list."`
	Account string `json:"account,omitempty"`
	Name    string `json:"name,omitempty" jsonschema:"Required to create a label"`
	ID      string `json:"id,omitempty" jsonschema:"Required to delete a label"`
}

func (t *Tools) handleLabels(ctx context.Context, _ *mcp.CallToolRequest, args labelsArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if args.Action == "" {
		args.Action = "list"
	}

	// Listing labels is reading; creating or deleting one changes the mailbox.
	needed := mail.CapRead
	if args.Action != "list" {
		needed = mail.CapLabels
	}

	var selector []string
	if args.Account != "" {
		selector = []string{args.Account}
	}
	accounts, err := t.gate.Resolve(ctx, g, "mail.labels", selector, needed)
	if err != nil {
		return toolError(err), nil, nil
	}
	if args.Action != "list" && len(accounts) > 1 {
		return t.invalid(ctx, g, grant.Audit{
			Tool: "mail.labels", Capability: needed, Detail: grant.Detail{Action: args.Action},
		}, fmt.Errorf("name one mailbox with `account` to %s a label", args.Action)), nil, nil
	}

	// Everything wrong with the call itself is settled before the first mailbox is touched.
	// Once the fan-out has started, a refusal that applies to every account equally would
	// still discard whatever the earlier ones had already done.
	invalidLabels := func(err error) *mcp.CallToolResult {
		return t.invalid(ctx, g, grant.Audit{
			Tool: "mail.labels", Capability: needed, Detail: grant.Detail{Action: args.Action},
		}, err)
	}
	switch args.Action {
	case "list":
	case "create":
		if args.Name == "" {
			return invalidLabels(fmt.Errorf("name is required to create a label")), nil, nil
		}
	case "delete":
		if args.ID == "" {
			return invalidLabels(fmt.Errorf("id is required to delete a label")), nil, nil
		}
	default:
		return invalidLabels(fmt.Errorf("action must be list, create or delete; got %q", args.Action)), nil, nil
	}

	outcomes := aggregate.NewOutcomes()
	for _, acct := range accounts {
		manager, err := t.labelManager(ctx, acct)
		if err != nil {
			outcomes.Fail(acct, err)
			continue
		}

		switch args.Action {
		case "list":
			labels, err := manager.ListLabels(ctx)
			if err := t.auditRead(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.labels", Capability: mail.CapRead,
				Affected: counted(len(labels)), Detail: grant.Detail{Action: "list"},
			}, err); err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			rendered := make([]map[string]any, len(labels))
			for i, l := range labels {
				rendered[i] = map[string]any{
					"id": string(l.ID), "name": l.Name, "kind": string(l.Kind),
					"exclusive": l.Exclusive, "unread": l.Unread, "total": l.Total,
				}
			}
			// Wrapped in a map rather than handed over as a bare array, because the entry
			// has to have room to say which mailbox these labels came from.
			outcomes.OK(acct, map[string]any{"labels": rendered})

		case "create":
			l, err := manager.CreateLabel(ctx, args.Name, false)
			note := t.auditChange(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.labels", Capability: mail.CapLabels,
				Detail: grant.Detail{Action: "create", Name: args.Name},
			}, err)
			if err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			outcomes.OK(acct, noted(map[string]any{"created": string(l.ID), "name": l.Name}, note))

		case "delete":
			// On three of the four providers a label is a folder, and deleting it takes the
			// mail inside it — permanently on IMAP, which has no bin. That is a destructive
			// act wearing the name of a filing one, so it is gated on the effect rather than
			// on the tool, the same way applying a label is.
			//
			// Asked per mailbox, because the same call can be a tag removal in one and a
			// folder deletion in another. A classification that could not be made has not
			// passed, so the mailbox fails rather than being deleted from on the assumption
			// that nothing here is a container.
			destroys, err := manager.DeletingDestroysMail(ctx, mail.LabelID(args.ID))
			if err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			if destroys {
				if !g.Caps.Has(mail.CapDestructive) {
					refusal := deleteRefusal(g, acct, mail.LabelID(args.ID))
					// Recorded against `destructive`, which is what was missing, rather than
					// against the capability the tool is registered under. A row reading
					// "mail.labels / labels" is what would let this sit unnoticed.
					_ = t.gate.Record(ctx, g, grant.Audit{
						AccountID: acct.ID, Tool: "mail.labels", Capability: mail.CapDestructive,
						Detail:  grant.Detail{Action: "delete", Name: args.ID},
						Outcome: mail.Code(refusal), Reason: refusal.Error(),
					})
					outcomes.Fail(acct, refusal)
					continue
				}
				if g.Mode.Holds() {
					held := fmt.Errorf(
						"nothing was done: deleting %s in %s destroys the mail filed under it, "+
							"and this connection holds destructive actions for the mailbox's "+
							"owner to approve", args.ID, acct.Alias)
					_ = t.gate.Record(ctx, g, grant.Audit{
						AccountID: acct.ID, Tool: "mail.labels", Capability: mail.CapDestructive,
						Detail:  grant.Detail{Action: "delete", Name: args.ID},
						Outcome: "refused", Reason: held.Error(),
					})
					outcomes.Fail(acct, held)
					continue
				}
			}

			err = manager.DeleteLabel(ctx, mail.LabelID(args.ID))
			note := t.auditChange(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.labels", Capability: mail.CapLabels,
				Detail: grant.Detail{Action: "delete", Name: args.ID},
			}, err)
			if err != nil {
				outcomes.Fail(acct, err)
				continue
			}
			outcomes.OK(acct, noted(map[string]any{"deleted": args.ID}, note))
		}
	}

	if outcomes.Failed() {
		return errorWithDetail("every mailbox failed", outcomes.Payload()), nil, nil
	}
	return result(outcomes.Payload())
}

// --- helpers ---

// groupByAccount authorizes every id and groups them by mailbox, so a batch spanning several
// mailboxes becomes one call per provider. Every id is checked: a batch is not a way to slip
// one unauthorized message past the gate alongside twenty authorized ones.
//
// An id that cannot be routed is rejected on its own rather than failing the batch, and
// comes back named in the result. Refusing every id because one was mistyped is not a
// stricter gate — the other nineteen were authorized — it is just the partial failure the
// tool contract promises never to inflict, paid for by the caller.
// Rejections are recorded as one row for the batch rather than one row per id. Fifty ids a
// grant cannot reach is one refused call, and a row each would let a single malformed request
// fill the page an operator reads by scanning it. The row carries the count and, bounded, the
// ids themselves — which is what tells "the client is holding stale ids" apart from "the
// client is probing mailboxes it was never granted".
func (t *Tools) groupByAccount(ctx context.Context, g *grant.Grant, tool string, ids []string, c mail.Capability, outcomes *aggregate.Outcomes) map[mail.Account][]mail.ScopedID {
	grouped := map[mail.Account][]mail.ScopedID{}
	var rejected []string
	var firstErr error
	reject := func(raw string, err error) {
		outcomes.Reject(raw, err)
		rejected = append(rejected, raw)
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, raw := range ids {
		id, err := mail.ParseScopedID(raw)
		if err != nil {
			reject(raw, err)
			continue
		}
		acct, err := t.gate.ResolveInBatch(ctx, g, id, c)
		if err != nil {
			reject(raw, err)
			continue
		}
		grouped[acct] = append(grouped[acct], id)
	}

	if firstErr != nil {
		_ = t.gate.Record(ctx, g, grant.Audit{
			Tool: tool, Capability: c, Outcome: grant.RefusedAs(firstErr),
			Reason:   firstErr.Error(),
			Affected: counted(len(rejected)),
			Detail:   grant.Detail{Action: "refused", IDs: rejected},
		})
	}
	return grouped
}

// sendEntry and draftEntry describe an outgoing message for the audit log: where it was
// addressed, what it was called, and nothing whatever of what it said.
//
// Recipients and subject are the deliberate exception to "never arguments". They are the
// operator's own outbound metadata, on the one capability whose effects cannot be taken back,
// and without them the log records that mail was sent and not to whom — which leaves the most
// dangerous thing a grant can do as the least accountable. The body stays out, here and
// everywhere: there is no field on grant.Detail that would carry one.
func sendEntry(acct mail.AccountID, args composeArgs) grant.Audit {
	e := composeEntry(acct, args)
	e.Tool, e.Capability = "mail.send", mail.CapSend
	return e
}

func draftEntry(acct mail.AccountID, action string, args composeArgs) grant.Audit {
	e := composeEntry(acct, args)
	e.Tool, e.Capability = "mail.draft", mail.CapDraft
	e.Detail.Action = action
	return e
}

func composeEntry(acct mail.AccountID, args composeArgs) grant.Audit {
	return grant.Audit{
		AccountID: acct,
		// How much, for a message, is how many people it reached.
		Affected: counted(len(args.To) + len(args.Cc) + len(args.Bcc)),
		Detail: grant.Detail{
			To: recipients(args.To), Cc: recipients(args.Cc), Bcc: recipients(args.Bcc),
			Subject: args.Subject,
		},
	}
}

// describeModify renders what a modify actually changed, compactly enough to sit on one row:
// the labels added and removed, and the flags set either way.
func describeModify(add, remove []mail.LabelID, flags mail.FlagUpdate) string {
	var parts []string
	for _, l := range add {
		parts = append(parts, "+"+string(l))
	}
	for _, l := range remove {
		parts = append(parts, "-"+string(l))
	}
	flag := func(name string, set *bool) {
		if set == nil {
			return
		}
		if *set {
			parts = append(parts, "+"+name)
		} else {
			parts = append(parts, "-"+name)
		}
	}
	flag("read", flags.Read)
	flag("starred", flags.Starred)
	return strings.Join(parts, " ")
}

func (t *Tools) outgoing(acct mail.Account, args composeArgs, replyTo mail.ScopedID, attachments []mail.Attachment) mail.Outgoing {
	return mail.Outgoing{
		Account:     acct.ID,
		InReplyTo:   replyTo,
		To:          toAddresses(args.To),
		Cc:          toAddresses(args.Cc),
		Bcc:         toAddresses(args.Bcc),
		Subject:     args.Subject,
		Body:        mail.Body{Text: args.Body, HTML: args.HTML},
		Attachments: attachments,
	}
}

// resolveAttachments turns the caller's attachment list into bytes, authorizing each one.
//
// A referenced attachment is a read of whichever mailbox holds it, which is not necessarily
// the mailbox being composed in. That read is authorized on its own terms: the grant must
// cover the source account and hold `attachments` on it. Possession of an id proves nothing —
// ids appear in search results, get quoted in conversation, and are trivially guessable in
// shape — so each one goes through the gate exactly as mail_get_attachment does.
//
// The account is taken from the id itself, so a caller cannot direct the read elsewhere by
// naming a different mailbox alongside it.
func (t *Tools) resolveAttachments(ctx context.Context, g *grant.Grant, inputs []attachmentInput) ([]mail.Attachment, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	out := make([]mail.Attachment, 0, len(inputs))
	var total int64

	for i, in := range inputs {
		switch in.sources() {
		case 0:
			return nil, fmt.Errorf("attachment %d has no content: supply blob_id, or from_message "+
				"with attachment_id, or content_base64", i+1)
		case 1:
		default:
			return nil, fmt.Errorf("attachment %d names more than one source; supply one or the other", i+1)
		}

		var att mail.Attachment
		var err error
		switch {
		case in.ContentBase64 != "":
			att, err = decodeInlineAttachment(i, in)
		case in.BlobID != "":
			att, err = t.uploadedAttachment(ctx, g, i, in)
		default:
			att, err = t.fetchAttachment(ctx, g, i, in)
		}
		if err != nil {
			return nil, err
		}

		total += int64(len(att.Content))
		if total > maxTotalAttachments {
			return nil, fmt.Errorf("attachments total more than %d MiB, which is over what the provider will accept",
				maxTotalAttachments>>20)
		}
		out = append(out, att)
	}
	return out, nil
}

// oversizedInline is shared by the cheap pre-decode guard and the exact post-decode check, so
// a caller gets the same explanation whichever one catches it — including the way out, which
// is the part that actually helps.
func oversizedInline(filename string) error {
	return fmt.Errorf(
		"attachment %q is larger than the %d MiB inline limit. Inline content travels inside the "+
			"request itself; attach something already in a mailbox with from_message instead",
		filename, maxInlineAttachment>>20)
}

func decodeInlineAttachment(i int, in attachmentInput) (mail.Attachment, error) {
	if in.Filename == "" {
		return mail.Attachment{}, fmt.Errorf("attachment %d needs a filename", i+1)
	}
	// Refuse before decoding, so an absurd payload is not allocated just to be rejected.
	// EncodedLen gives the exact encoded size of a maximal attachment, so anything longer
	// than that must decode to something over the limit — and anything shorter is let
	// through to the precise check below rather than being caught by an estimate.
	if len(in.ContentBase64) > base64.StdEncoding.EncodedLen(maxInlineAttachment) {
		return mail.Attachment{}, oversizedInline(in.Filename)
	}

	content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		// Tolerate the unpadded form: it is a common way for base64 to arrive.
		content, err = base64.RawStdEncoding.DecodeString(in.ContentBase64)
	}
	if err != nil {
		return mail.Attachment{}, fmt.Errorf("attachment %q is not valid base64", in.Filename)
	}
	if int64(len(content)) > maxInlineAttachment {
		return mail.Attachment{}, oversizedInline(in.Filename)
	}

	mimeType := in.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return mail.Attachment{
		AttachmentRef: mail.AttachmentRef{
			Filename: in.Filename,
			MimeType: mimeType,
			Size:     int64(len(content)),
		},
		Content: content,
	}, nil
}

// uploadedAttachment reads back bytes the client PUT to an upload URL.
//
// Scoped to the owner and to the grant that minted the upload, which is the same rule the
// signed URLs follow: a blob is reachable by the grant that made it and by nothing else, so
// one client's staged file cannot be attached by another client's token, and a revoked grant
// cannot send what it staged before it was revoked.
func (t *Tools) uploadedAttachment(ctx context.Context, g *grant.Grant, i int, in attachmentInput) (mail.Attachment, error) {
	if t.blobs == nil {
		return mail.Attachment{}, fmt.Errorf("this server has no attachment storage configured, " +
			"so there are no uploads to attach")
	}

	ref, content, err := t.blobs.Content(ctx, g.OwnerID, in.BlobID)
	if err != nil {
		return mail.Attachment{}, stagedBlobError(i, in.BlobID, err)
	}
	if ref.GrantID != g.ID {
		// Reported as missing rather than forbidden: confirming that a blob id exists under a
		// different grant is itself a disclosure.
		return mail.Attachment{}, stagedBlobError(i, in.BlobID, blob.ErrNotFound)
	}

	filename := in.Filename
	if filename == "" {
		filename = ref.Filename
	}
	mimeType := in.MimeType
	if mimeType == "" {
		mimeType = ref.MimeType
	}
	return mail.Attachment{
		AttachmentRef: mail.AttachmentRef{
			Filename: filename, MimeType: mimeType, Size: int64(len(content)),
		},
		Content: content,
	}, nil
}

// stagedBlobError explains why a blob_id did not resolve, in terms of what to do about it.
//
// The sentinel is kept underneath with %w so errors.Is still answers, and so the audit row
// records the same failure it always did. What changes is the sentence the model reads. The
// bare sentinels are accurate and useless on their own: "blob has expired" and "no such blob"
// both read as "the file is gone", and the correct response to each is the same three steps
// nobody is told about — call mail_upload_url again, PUT the bytes, name the new blob_id.
//
// Expiry is separated from a miss because only one of them is worth explaining twice. A
// staged upload is deleted once MAILROOM_ATTACHMENT_TTL passes, so a blob_id that worked
// earlier in a long conversation is the expected way to reach this, and a caller told "not
// found" would go looking for a typo instead of re-uploading.
func stagedBlobError(i int, blobID string, err error) error {
	switch {
	case errors.Is(err, blob.ErrGone):
		return fmt.Errorf("attachment %d: the upload %s expired before it was attached. "+
			"Staged files are deleted after a while, and this one is past that — the bytes are "+
			"gone from this server, though whatever you uploaded them from is untouched. Call "+
			"mail_upload_url again, PUT the file to the new URL, and name the new blob_id: %w",
			i+1, blobID, err)
	case errors.Is(err, blob.ErrNotReady):
		return fmt.Errorf("attachment %d: %s was reserved by mail_upload_url but no bytes ever "+
			"arrived — the PUT to its upload_url did not complete. Perform that PUT and call "+
			"this again, or mint a fresh URL if the old one has expired or been used: %w",
			i+1, blobID, err)
	case errors.Is(err, blob.ErrNotFound):
		return fmt.Errorf("attachment %d: nothing is staged under %s. The likeliest reason is "+
			"that it expired — staged uploads are deleted after a while, and once one has been "+
			"swept this server cannot tell an expired id from one that never existed. It may "+
			"also belong to a different connection: a blob_id is usable only by the grant that "+
			"created it. Either way there is nothing to recover — call mail_upload_url, PUT the "+
			"file to the new URL, and name the new blob_id: %w", i+1, blobID, err)
	default:
		return fmt.Errorf("attachment %d (%s): %w", i+1, blobID, err)
	}
}

// fetchAttachment reads an attachment out of whichever mailbox holds it, after checking the
// grant covers that mailbox for `attachments`.
func (t *Tools) fetchAttachment(ctx context.Context, g *grant.Grant, i int, in attachmentInput) (mail.Attachment, error) {
	if in.AttachmentID == "" {
		return mail.Attachment{}, fmt.Errorf("attachment %d names from_message but no attachment_id", i+1)
	}
	source, err := mail.ParseScopedID(in.FromMessage)
	if err != nil {
		return mail.Attachment{}, fmt.Errorf("attachment %d: %w", i+1, err)
	}

	// The source mailbox is authorized independently of the one being composed in. Attaching
	// across mailboxes is a read of the source, and needs the capability there.
	acct, err := t.gate.ResolveOne(ctx, g, "mail.get_attachment", source, mail.CapAttachments)
	if err != nil {
		return mail.Attachment{}, err
	}
	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return mail.Attachment{}, err
	}
	reader, ok := p.(mail.AttachmentReader)
	if !ok {
		return mail.Attachment{}, &mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address,
			Capability: mail.CapAttachments,
		}
	}

	att, err := reader.GetAttachment(ctx, source, in.AttachmentID)
	if err := t.auditRead(ctx, g, grant.Audit{
		AccountID: acct.ID, Tool: "mail.get_attachment", Capability: mail.CapAttachments,
		Affected: counted(1),
		Detail:   grant.Detail{Action: "attach", IDs: []string{source.String()}},
	}, err); err != nil {
		return mail.Attachment{}, err
	}

	// Let the caller rename or retype it; otherwise keep what the source said.
	if in.Filename != "" {
		att.Filename = in.Filename
	}
	if in.MimeType != "" {
		att.MimeType = in.MimeType
	}
	if att.Filename == "" {
		att.Filename = "attachment"
	}
	att.Size = int64(len(att.Content))
	return att, nil
}

func (t *Tools) draftManager(ctx context.Context, acct mail.Account) (mail.DraftManager, error) {
	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return nil, err
	}
	drafts, ok := p.(mail.DraftManager)
	if !ok {
		return nil, &mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address, Capability: mail.CapDraft,
		}
	}
	return drafts, nil
}

// labelManager is the only way a tool reaches a provider's labels, and it hands back a
// guarded manager rather than the provider's own. See destructive_labels.go for what the
// guard refuses and why it is here rather than only in the handler.
func (t *Tools) labelManager(ctx context.Context, acct mail.Account) (mail.LabelManager, error) {
	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return nil, err
	}
	labels, ok := p.(mail.LabelManager)
	if !ok {
		return nil, &mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address, Capability: mail.CapLabels,
		}
	}
	return guardedLabels{LabelManager: labels, acct: acct}, nil
}

func toLabelIDs(in []string) []mail.LabelID {
	out := make([]mail.LabelID, len(in))
	for i, s := range in {
		out[i] = mail.LabelID(s)
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
