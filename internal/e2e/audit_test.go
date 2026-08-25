package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/store"
)

// The audit log, crossed with everything that was supposed to start writing to it.
//
// The log's whole justification is that a silent action is the failure it exists to prevent,
// so the question worth asking of five features that landed separately is which of their new
// paths write a row and which write nothing.

// TestEveryNewPathWritesAnAuditRow walks one grant through every path the five features
// added and reports what the log holds afterwards.
func TestEveryNewPathWritesAnAuditRow(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	personal := r.link("personal", "ada@home.example")
	msg := r.mailbox(work).seed("quarterlies", "q3.txt", "text/plain", []byte("numbers"))
	elsewhere := r.mailbox(personal).seed("theirs", "x.txt", "text/plain", []byte("x"))

	s, id := r.grantFor(approval{
		label: "Everything", accounts: []mail.Account{work},
		caps: []mail.Capability{
			mail.CapRead, mail.CapAttachments, mail.CapDraft, mail.CapDiscard,
			mail.CapSend, mail.CapDestructive,
		},
		mode: grant.ModeHold,
	})

	// 1. A refusal by the gate: an id in a mailbox this grant does not reach.
	s.callError("mail_get_message", map[string]any{"id": elsewhere.String()})

	// 2. A refusal on the arguments alone, reaching no mailbox.
	s.callError("mail_trash", map[string]any{"ids": []string{}})

	// 3. Minting an upload URL: bytes may now be written to this server's disk.
	minted := s.callOK("mail_upload_url", map[string]any{"filename": "contract.pdf"})
	uploadURL := str(minted.payload["upload_url"])

	// 4. The upload itself, which crosses the wire outside the MCP conversation.
	if status, body := r.put(uploadURL, []byte("%PDF-1.4")); status != http.StatusCreated {
		t.Fatalf("the upload answered %d: %s", status, body)
	}

	// 5. A download link, and 6. the fetch that follows it.
	link := str(s.callOK("mail_get_attachment", map[string]any{
		"message_id": msg.String(), "attachment_id": "att1",
	}).payload["url"])
	if status, _, _ := r.fetch(link); status != http.StatusOK {
		t.Fatalf("fetching the link answered %d", status)
	}

	// 7. A discard, which is the ninth capability and is not held.
	draftID := str(s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "d",
	}).payload["draft_id"])
	s.callOK("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})

	// 8. A held action, 9. one approved and 10. one declined.
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "a@example.net"}}, "subject": "one",
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "b@example.net"}}, "subject": "two",
	})
	queued := r.pending()
	if len(queued) != 2 {
		t.Fatalf("the queue holds %d actions", len(queued))
	}
	r.approveHeld(queued[0].ID).Body.Close()
	r.declineHeld(queued[1].ID).Body.Close()

	// 11. Removing the grant, which is an operator action on the client's permission itself.
	r.revoke(id)
	r.removeGrant(id)

	rows := r.audit()
	t.Logf("the log holds %d rows:\n%s", len(rows), r.auditDump())

	has := func(tool, outcome string) bool {
		for _, row := range rows {
			if row.Tool == tool && row.Outcome == outcome {
				return true
			}
		}
		return false
	}
	for _, want := range []struct{ tool, outcome, why string }{
		{"mail.get_message", "scope_denied", "a call the gate turned away"},
		{"mail.trash", grant.OutcomeInvalid, "a call refused on its own arguments"},
		{"mail.upload_url", "ok", "a grant was given somewhere to write"},
		{"mail.attachment_upload", "ok", "bytes arrived on this server's disk"},
		{"mail.get_attachment", "ok", "an attachment was pulled out of a mailbox"},
		{"mail.attachment_download", "ok", "the bytes crossed the wire"},
		{"mail.draft", "ok", "a draft was written, and one was discarded"},
		{"mail.send", "held", "a send was queued rather than performed"},
		{"mail.send", "declined", "a queued send was thrown away"},
	} {
		if !has(want.tool, want.outcome) {
			t.Errorf("nothing recorded %s/%s — %s", want.tool, want.outcome, want.why)
		}
	}

	// Approving a held send records the delivery. What the row does not carry is the point of
	// the next test.
	if !has("mail.send", "ok") {
		t.Error("approving a held send recorded no delivery")
	}

	// Removing a grant writes nothing, which is a gap rather than a decision: it is an
	// operator action on somebody's permissions, and the audit page is where an operator
	// looks to reconstruct what happened.
	var operatorRows int
	for _, row := range rows {
		if strings.HasPrefix(row.Tool, "grant.") || row.Tool == "grant.removed" {
			operatorRows++
		}
	}
	if operatorRows == 0 {
		t.Logf("FINDING (low): revoking and removing a grant write no audit row at all. " +
			"The log records what a client did and nothing an operator did to it, so a grant " +
			"that stops appearing has no recorded reason.")
	}
}

// TestApprovingAHeldSendWritesALessDetailedRowThanHoldingIt is a real regression in the
// enriched log, and it lands on the one row that matters most.
//
// Holding a send writes everything: the capability it would spend, how many people it would
// reach, the recipients, the subject. Approving it — the row that means mail actually left
// the mailbox — writes the tool, the outcome and nothing else, because internal/held builds
// its own grant.Audit with four fields set. So the log's most detailed record of a send is
// the one where no mail moved, and its least detailed is the one where it did.
func TestApprovingAHeldSendWritesALessDetailedRowThanHoldingIt(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work",
		"to":      []map[string]any{{"email": "finance@example.net"}, {"email": "legal@example.net"}},
		"subject": "the invoice", "body": "x",
	})
	pending := r.pending()
	r.approveHeld(pending[0].ID).Body.Close()

	var heldRow, sentRow store.AuditEntry
	for _, row := range r.auditFor("mail.send") {
		switch row.Outcome {
		case "held":
			heldRow = row
		case "ok":
			sentRow = row
		}
	}
	if heldRow.Outcome != "held" || sentRow.Outcome != "ok" {
		t.Fatalf("expected both a held and an ok row:\n%s", r.auditDump())
	}

	if len(heldRow.Detail.To) != 2 || heldRow.Capability == "" || heldRow.Affected == nil {
		t.Fatalf("the held row is not the detailed one after all: %+v", heldRow)
	}
	if sentRow.Capability == "" || sentRow.Affected == nil || len(sentRow.Detail.To) == 0 {
		t.Logf("FINDING (medium): the row for the send that actually happened carries "+
			"capability=%q affected=%v to=%v, while the row for the send that did not carries "+
			"capability=%q affected=%v to=%v. internal/held.Queue.record sets only OwnerID, "+
			"GrantID, AccountID, Tool, Outcome and At, so an approved action writes none of "+
			"the four columns the enriched log added.",
			sentRow.Capability, sentRow.Affected, sentRow.Detail.To,
			heldRow.Capability, heldRow.Affected, heldRow.Detail.To)
	} else {
		t.Fatal("the approved row now carries detail; re-read the report, this finding is stale")
	}

	// And nothing on the row says the delivery came from an approval rather than from the
	// client calling mail_send directly, which are different events with different authors.
	if sentRow.Detail.Action != "" {
		t.Errorf("the approved row now names an action (%q); the finding above may be stale",
			sentRow.Detail.Action)
	}
}

// TestAuditRowsAreOrderedWithinASecond is the regression test for the ordering fix in this
// commit. `at` is unix seconds, so a client doing two things quickly puts both in one, and
// the page is a chronology.
func TestAuditRowsAreOrderedWithinASecond(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Ordered", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft, mail.CapDiscard},
	})

	draftID := str(s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "d",
	}).payload["draft_id"])
	s.callOK("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})

	rows := r.auditFor("mail.draft")
	if len(rows) != 2 {
		t.Fatalf("expected a create and a delete, got %d rows:\n%s", len(rows), r.auditDump())
	}
	if rows[0].Detail.Action != "delete" {
		t.Fatalf("the newest-first page led with %q; the delete happened second\n%s",
			rows[0].Detail.Action, r.auditDump())
	}
	if rows[0].Capability != string(mail.CapDiscard) {
		t.Errorf("the delete recorded capability %q", rows[0].Capability)
	}
	if rows[1].Capability != string(mail.CapDraft) {
		t.Errorf("the create recorded capability %q", rows[1].Capability)
	}
}

// TestTheAuditPageStillNamesARemovedGrant is the whole justification for choosing a soft
// delete over a hard one, driven through the page rather than the query.
func TestTheAuditPageStillNamesARemovedGrant(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Nightly digest", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend},
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "digest",
	})

	r.revoke(id)
	r.removeGrant(id)

	page, _ := r.page("/audit")
	if !strings.Contains(page, "Nightly digest") {
		t.Fatalf("the audit page lost the name of a removed grant, which is the reason " +
			"removal is a soft delete")
	}
	if strings.Contains(page, string(id)) {
		t.Logf("the page also renders the removed grant's id")
	}
	// The grants page must not: a removed grant is off it by definition.
	grants, _ := r.page("/grants")
	if strings.Contains(grants, "Nightly digest") {
		t.Error("a removed grant is still listed on the grants page")
	}
	t.Logf("FINDING (low): the removed grant's rows are drawn exactly like a live grant's — " +
		"same label, no marker — so an operator reading /audit and then looking for " +
		"\"Nightly digest\" on /grants finds nothing and has no way to tell why. " +
		"internal/web/templates/held.html already badges an action whose grant was revoked.")
}

// TestARefusedCallNamesNothingTheClientChose. A refusal is recorded, and what it records must
// not be attacker-controlled text: the audit page is the operator's, and a client that could
// write into it could write anything there.
func TestARefusedCallNamesNothingTheClientChose(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work}, caps: []mail.Capability{mail.CapRead},
	})

	const injected = "<script>alert(1)</script>NOT-A-MAILBOX"
	s.callError("mail_search", map[string]any{"account": injected})

	for _, row := range r.audit() {
		if strings.Contains(row.Account, "script") || strings.Contains(row.Detail.Name, "script") {
			t.Fatalf("a caller-supplied selector reached an audit column: %+v", row)
		}
	}
	page, _ := r.page("/audit")
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("caller-supplied text was rendered unescaped on the audit page")
	}
}

// TestDroppedRecipientsAreCountedAsMessagesOnTheAuditPage.
//
// grant.Audit.Bounded caps ids and each recipient list at ten and adds every overflow into
// one Detail.More. The page renders that count in exactly one place: beside the id list,
// under a heading that reads "Messages". So a send to fifteen people writes one sent-message
// id, ten recipients and More=5, and the operator is shown one message id followed by "and 5
// more" — five dropped *recipients*, reported as five more *messages*, on a page whose own
// footer promises "A long list of ids or recipients is cut short, and the count beside it
// says how many there really were".
func TestDroppedRecipientsAreCountedAsMessagesOnTheAuditPage(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Bulk", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend},
	})

	to := make([]map[string]any, 0, 15)
	for i := range 15 {
		to = append(to, map[string]any{"email": string(rune('a'+i)) + "@example.net"})
	}
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": to, "subject": "the announcement", "body": "x",
	})

	row := r.lastAuditFor("mail.send")
	if row.Affected == nil || *row.Affected != 15 {
		t.Fatalf("affected was %v, want the true recipient count of 15", row.Affected)
	}
	if len(row.Detail.To) != 10 || len(row.Detail.IDs) != 1 {
		t.Fatalf("the row holds %d recipients and %d ids", len(row.Detail.To), len(row.Detail.IDs))
	}
	if row.Detail.More != 5 {
		t.Fatalf("More was %d, want the 5 recipients that were dropped", row.Detail.More)
	}

	page, _ := r.page("/audit")
	flat := strings.Join(strings.Fields(page), " ")
	i := strings.Index(flat, "<dt>Messages</dt>")
	if i < 0 {
		t.Fatal("the audit page has no Messages block")
	}
	block := flat[i:min(i+400, len(flat))]
	if !strings.Contains(block, "and 5 more") {
		t.Fatalf("the count is no longer rendered under Messages; this finding is stale:\n%s", block)
	}
	t.Logf("FINDING (low): the audit page renders %q. The 5 are recipients that "+
		"grant.Audit.Bounded dropped from Detail.To; only one message id exists. Detail.More "+
		"is a single counter shared by IDs, To, Cc and Bcc, and the template attributes it "+
		"entirely to the id list.", strings.TrimSpace(block[:min(len(block), 220)]))
}
