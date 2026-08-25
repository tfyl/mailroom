package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// The ninth capability, crossed with the grants that predate it and with the tools that
// reach the same effect by another route.

// TestAGrantFromBeforeTheSplitCanDraftAndNotDiscard.
//
// Nothing back-fills `discard` onto an existing grant, so every grant approved before the
// split holds `draft` alone. That is the safe direction and it has to actually work: the
// draft tool must still be offered, creating and updating must still run, and only the delete
// must be refused — with a refusal that names what is missing, because the client's only way
// out is to ask for it.
func TestAGrantFromBeforeTheSplitCanDraftAndNotDiscard(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Pre-split", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft},
	})

	if names := s.toolNames(); !slices.Contains(names, "mail_draft") {
		t.Fatalf("a draft-only grant was offered %v", names)
	}
	created := s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "a draft", "body": "text",
	})
	draftID := str(created.payload["draft_id"])

	s.callOK("mail_draft", map[string]any{
		"action": "update", "draft_id": draftID, "subject": "revised", "body": "more",
	})

	refused := s.callError("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})
	if refused.payload["capability"] != string(mail.CapDiscard) {
		t.Errorf("the refusal did not name `discard` as what is missing:\n%s", refused.text)
	}
	held, _ := refused.payload["held"].([]any)
	if len(held) != 1 || held[0] != string(mail.CapDraft) {
		t.Errorf("the refusal did not report what the grant does hold: %v", held)
	}
	if r.mailbox(work).draftCount() != 1 {
		t.Error("the draft was removed by a grant that does not hold `discard`")
	}

	row := r.lastAuditFor("mail.draft")
	if row.Outcome == "ok" {
		t.Errorf("the refused delete was recorded as ok:\n%s", r.auditDump())
	}
	if row.Capability != string(mail.CapDiscard) {
		t.Errorf("the refusal was recorded against capability %q", row.Capability)
	}
}

// TestDiscardWithoutDraftIsUsable is the other half of the split, which the consent screen
// offers and which the tool registration has to honour.
func TestDiscardWithoutDraftIsUsable(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	// Something to discard, written by a different grant.
	writer, _ := r.grantFor(approval{
		label: "Writer", accounts: []mail.Account{work}, caps: []mail.Capability{mail.CapDraft},
	})
	draftID := str(writer.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "d",
	}).payload["draft_id"])

	cleaner, _ := r.grantFor(approval{
		label: "Cleaner", accounts: []mail.Account{work}, caps: []mail.Capability{mail.CapDiscard},
	})
	if names := cleaner.toolNames(); !slices.Contains(names, "mail_draft") {
		t.Fatalf("a discard-only grant was offered %v, so it has no way to delete a draft", names)
	}
	cleaner.callOK("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})
	if r.mailbox(work).draftCount() != 0 {
		t.Fatal("a discard-only grant could not remove a draft")
	}

	refused := cleaner.callError("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "new",
	})
	if refused.payload["capability"] != string(mail.CapDraft) {
		t.Errorf("creating was not refused for want of `draft`:\n%s", refused.text)
	}
}

// TestSendingADraftRemovesItWithoutDiscard is a deliberate decision with a documentation
// problem attached.
//
// mail_send with a draft_id needs `send` and nothing else, and every provider removes the
// draft as part of delivering it. That is defensible — a draft that became a message was not
// thrown away — and it is documented for developers in docs/grants.md. What it contradicts is
// the sentence an operator reads while deciding, on the consent screen itself.
func TestSendingADraftRemovesItWithoutDiscard(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Composer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft, mail.CapSend},
	})

	draftID := str(s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "the reply", "body": "text",
	}).payload["draft_id"])
	if r.mailbox(work).draftCount() != 1 {
		t.Fatal("the draft did not reach the mailbox")
	}

	s.callOK("mail_send", map[string]any{"draft_id": draftID})
	if r.mailbox(work).draftCount() != 0 {
		t.Fatal("sending a draft left it in the mailbox")
	}

	// The consent screen the operator approved this on says otherwise, in as many words.
	c := r.register("Reader of the consent screen")
	page := strings.Join(strings.Fields(r.consentPage(c)), " ")
	const claim = "Without it an agent can write and revise drafts and cannot throw one away"
	if strings.Contains(page, claim) {
		t.Logf("FINDING (medium): the consent screen says a grant without `discard` "+
			"%q — the exact words are in internal/web/templates/consent.html. A grant "+
			"holding `draft` and `send` and not `discard` just removed a draft, by sending "+
			"it. The behaviour is deliberate (internal/mcp/tools_write.go documents it); the "+
			"sentence the operator decides on is what is wrong.", claim)
	} else {
		t.Error("the consent screen no longer makes that claim; this finding is stale and " +
			"the test should be updated")
	}
}

// The destructive-label class. mail_trash needs `destructive` and is held under `hold`;
// mail_modify needs `labels` and used to be held under nothing. On every provider that has a
// bin, adding the TRASH label through mail_modify is the same operation as trashing: Gmail's
// BatchModify moves the message (internal/provider/gmail/write.go), the IMAP provider
// implements a label add as a MOVE into the named mailbox (internal/provider/imap/write.go),
// and Zoho and Graph do the same with a folder id. mail_filters' own description says the
// equivalence out loud — "trashing is adding TRASH".
//
// So the rule these four tests hold to the fire is one sentence: a label operation whose
// effect is destruction needs `destructive` as well as `labels`, and is held exactly as
// mail_trash is.

// TestATrashingLabelNeedsDestructiveAsWellAsLabels is the reproduction of the finding, read
// the right way round. A grant holding `labels` and not `destructive` must not be able to bin
// somebody's mail, in any mode.
func TestATrashingLabelNeedsDestructiveAsWellAsLabels(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("an invoice", "f.txt", "text/plain", []byte("f"))

	s, _ := r.grantFor(approval{
		label: "Filer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapLabels}, mode: grant.ModeHold,
	})

	// The tool that says it does this is not even offered, because the capability is absent.
	if names := s.toolNames(); slices.Contains(names, "mail_trash") {
		t.Fatalf("a grant without `destructive` was offered mail_trash: %v", names)
	}

	refused := s.callError("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "add_labels": []string{"TRASH"},
	})
	if refused.payload["capability"] != string(mail.CapDestructive) {
		t.Errorf("the refusal did not name `destructive` as what is missing:\n%s", refused.text)
	}
	held, _ := refused.payload["held"].([]any)
	if len(held) != 2 {
		t.Errorf("the refusal did not report what the grant does hold: %v", held)
	}

	if trashed := r.mailbox(work).snapshot().trashed; trashed != 0 {
		t.Fatalf("the provider was handed %d trashings by a grant without `destructive`", trashed)
	}
	if len(r.pending()) != 0 {
		t.Fatalf("a refused call was queued instead: %+v", r.pending())
	}

	// The audit row says what was refused and why, against the capability that was missing
	// rather than against the one the tool is registered under.
	row := r.lastAuditFor("mail.modify")
	if row.Capability != string(mail.CapDestructive) {
		t.Errorf("mail.modify recorded capability %q for a refusal about `destructive`", row.Capability)
	}
	if row.Outcome == "ok" {
		t.Errorf("the refused modify was recorded as ok:\n%s", r.auditDump())
	}
	if !strings.Contains(row.Detail.Action, "TRASH") {
		t.Errorf("the audit row does not name the label that did it: %q", row.Detail.Action)
	}

	// Junk is the same act with a filter attached, and is refused on the same terms.
	junk := s.callError("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "add_labels": []string{"Junk"},
	})
	if junk.payload["capability"] != string(mail.CapDestructive) {
		t.Errorf("moving mail to junk was not refused for want of `destructive`:\n%s", junk.text)
	}
	if junked := r.mailbox(work).snapshot().junked; junked != 0 {
		t.Fatalf("%d messages were moved to junk by a grant without `destructive`", junked)
	}
}

// TestATrashingLabelIsHeldUnderHold is the mode half. `hold` is the one mode with teeth, and
// its promise is that nothing destructive happens until the mailbox's owner says so — by any
// route, not only through the tool named after it.
func TestATrashingLabelIsHeldUnderHold(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("an invoice", "f.txt", "text/plain", []byte("f"))

	s, _ := r.grantFor(approval{
		label: "Filer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapLabels, mail.CapDestructive},
		mode: grant.ModeHold,
	})

	res := s.callOK("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "add_labels": []string{"TRASH"},
	})
	if !strings.Contains(res.text, "held") {
		t.Fatalf("a trashing label change was performed rather than held:\n%s", res.text)
	}
	if trashed := r.mailbox(work).snapshot().trashed; trashed != 0 {
		t.Fatalf("the provider was handed %d trashings under `hold`", trashed)
	}

	pending := r.pending()
	if len(pending) != 1 {
		t.Fatalf("the queue holds %d actions: %+v", len(pending), pending)
	}
	if !strings.Contains(pending[0].Summary, "TRASH") {
		t.Errorf("the queued summary does not say what it would do: %q", pending[0].Summary)
	}

	// And the operator's own press is what finally moves the mail.
	resp := r.approveHeld(pending[0].ID)
	defer resp.Body.Close()
	if trashed := r.mailbox(work).snapshot().trashed; trashed != 1 {
		t.Fatalf("approving the held label change trashed %d messages", trashed)
	}
	if len(r.pending()) != 0 {
		t.Error("the action is still pending after being approved")
	}
}

// TestLabelsAndDestructiveTogetherCanTrashByLabel. The check is a check, not a ban: a grant
// that holds both capabilities, in a mode that does not hold, does the thing it was granted.
func TestLabelsAndDestructiveTogetherCanTrashByLabel(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("an invoice", "f.txt", "text/plain", []byte("f"))

	s, _ := r.grantFor(approval{
		label: "Filer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapLabels, mail.CapDestructive},
	})

	s.callOK("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "add_labels": []string{"TRASH"},
	})
	if trashed := r.mailbox(work).snapshot().trashed; trashed != 1 {
		t.Fatalf("a grant holding both capabilities trashed %d messages", trashed)
	}
	if len(r.pending()) != 0 {
		t.Fatalf("a grant not in `hold` had its call queued: %+v", r.pending())
	}
}

// TestOrdinaryLabelWorkStillNeedsLabelsAlone is the other side of the bargain, and the reason
// the check is on the effect rather than on the tool. Filing, archiving, starring and marking
// read are what `labels` is for, and none of them lose anything.
func TestOrdinaryLabelWorkStillNeedsLabelsAlone(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("an invoice", "f.txt", "text/plain", []byte("f"))

	s, _ := r.grantFor(approval{
		label: "Filer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapLabels}, mode: grant.ModeHold,
	})

	s.callOK("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "add_labels": []string{"Receipts"},
		"archive": true, "read": true,
	})
	if len(r.pending()) != 0 {
		t.Fatalf("ordinary filing was queued under `hold`: %+v", r.pending())
	}
	state := r.mailbox(work).snapshot()
	if state.trashed != 0 || state.junked != 0 {
		t.Fatalf("ordinary filing reached the bin: %+v", state)
	}

	// Putting mail back is never destructive, so it stays inside `labels` too: the check is
	// on what is applied, and removing TRASH is a restore.
	s.callOK("mail_modify", map[string]any{
		"ids": []string{msg.String()}, "remove_labels": []string{"TRASH"},
	})
}

// TestAFilterThatBinsMailNeedsDestructive is the same rule on the tool that applies labels to
// mail that has not arrived yet.
//
// mail_filters' own description says the equivalence out loud — "trashing is adding TRASH" —
// and it is the stronger case of the two: a rule runs unattended, repeatedly, on mail nobody
// has read. `filters` alone was enough to write one.
func TestAFilterThatBinsMailNeedsDestructive(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	s, _ := r.grantFor(approval{
		label: "Rule writer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapFilters},
	})

	refused := s.callError("mail_filters", map[string]any{
		"account": "work", "action": "create",
		"from": "noreply@example.net", "add_labels": []string{"TRASH"},
	})
	if refused.payload["capability"] != string(mail.CapDestructive) {
		t.Errorf("a filter that bins mail was not refused for want of `destructive`:\n%s", refused.text)
	}
	if filters := r.mailbox(work).snapshot().filters; filters != 0 {
		t.Fatalf("%d filters were written by a grant without `destructive`", filters)
	}

	// A filter that files rather than bins is what `filters` is for, and still works alone.
	s.callOK("mail_filters", map[string]any{
		"account": "work", "action": "create",
		"from": "receipts@example.net", "add_labels": []string{"Receipts"},
	})
	if filters := r.mailbox(work).snapshot().filters; filters != 1 {
		t.Fatal("an ordinary filing rule was refused")
	}
}

// TestModeIsNotSettableFromTheClient. The mode is what makes hold mean anything, so a client
// that could write it would have no mode at all.
func TestModeIsNotSettableFromTheClient(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})

	// Every registered tool, driven with a mode argument bolted on. The schemas are generated
	// with additionalProperties:false, so this is refused by the transport rather than
	// reaching a handler — which is the enforcement, and is worth demonstrating rather than
	// assuming.
	for _, name := range s.toolNames() {
		res := s.call(name, map[string]any{"mode": "unattended"})
		if !res.isError {
			t.Errorf("%s accepted a `mode` argument:\n%s", name, res.text)
		}
	}

	stored, err := r.db.Grant(r.ctx, id)
	if err != nil {
		t.Fatalf("re-reading the grant: %v", err)
	}
	if stored.Mode != grant.ModeHold {
		t.Fatalf("the stored mode changed to %q", stored.Mode)
	}
}

// TestAnUnrecognisedModeIsRefusedAtTheConsentScreen. A form that has drifted from the server
// must not quietly land on a default, in either direction.
func TestAnUnrecognisedModeIsRefusedAtTheConsentScreen(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	c := r.register("Drifted")

	page := r.consentPage(c)
	requestID := attrValue(page, "request_id")
	form := map[string][]string{
		"csrf_token":   {csrfFrom(page)},
		"request_id":   {requestID},
		"label":        {"Drifted"},
		"expires_days": {"never"},
		"mode":         {"whenever"},
		"accounts":     {string(work.ID)},
		"capabilities": {string(mail.CapRead)},
	}
	resp := r.post("/authorize/approve", form)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("an unknown mode was accepted with %d", resp.StatusCode)
	}
	grants, err := r.db.ListGrants(r.ctx, r.owner.ID)
	if err != nil || len(grants) != 0 {
		t.Fatalf("a grant was recorded anyway: %v %v", grants, err)
	}
}

// TestHoldOnAGrantWithNothingHoldableIsAccepted.
//
// Nothing validates the mode against the capabilities, in either the consent screen or the
// edit page. The result is a grant whose mode is enforced against nothing, presented to the
// operator and to the client as though it were doing work.
func TestHoldOnAGrantWithNothingHoldableIsAccepted(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	s, id := r.grantFor(approval{
		label: "Inert hold", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapDraft, mail.CapDiscard, mail.CapLabels},
		mode: grant.ModeHold,
	})
	stored, err := r.db.Grant(r.ctx, id)
	if err != nil || stored.Mode != grant.ModeHold {
		t.Fatalf("the grant recorded mode %v (%v)", stored, err)
	}

	res := s.callOK("mail_accounts", map[string]any{})
	mode, _ := res.payload["mode"].(map[string]any)
	if mode["enforced"] != true {
		t.Fatalf("mode reported %+v", mode)
	}
	t.Logf("FINDING (low): `hold` was accepted on a grant holding none of the four " +
		"capabilities it can hold (send, destructive, filters, settings). Nothing will ever " +
		"be queued, and both the consent screen and mail_accounts report the mode as " +
		"enforced. Neither internal/oauthsrv.Approve nor the grant edit page compares the " +
		"mode against the capability set.")

	// The edit page will take it too, which is the other half of the same gap.
	if err := r.db.EditGrant(r.ctx, r.owner.ID, id, []mail.AccountID{work.ID},
		mail.NewSet(mail.CapRead), grant.ModeHold, nil); err != nil {
		t.Fatalf("editing to a read-only grant in hold mode: %v", err)
	}
}

// TestExpiredGrantStopsItsToken closes the loop on the third grant state. Expiry is re-read
// per request like revocation, so an edit that moves the expiry into the past has to take the
// token down with it.
func TestExpiredGrantStopsItsToken(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, token, id := r.grantWithToken(approval{
		label: "Short-lived", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead},
	})
	s.callOK("mail_accounts", map[string]any{})

	past := time.Now().Add(-time.Hour)
	if err := r.db.EditGrant(r.ctx, r.owner.ID, id,
		[]mail.AccountID{work.ID}, mail.NewSet(mail.CapRead), grant.ModeConfirm, &past); err != nil {
		t.Fatalf("expiring the grant: %v", err)
	}
	if err := r.connectExpectingFailure(token); err == nil {
		t.Fatal("an expired grant's token still completed MCP initialize")
	}
	if err := r.connectExpectingFailure("not-a-token"); err == nil {
		t.Fatal("a token this server never issued was accepted")
	}
}
