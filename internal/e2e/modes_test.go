package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/store"
)

// Modes crossed with everything else.
//
// Each of these asks a question that neither feature's own tests can answer, because the
// answer lives in the seam: what the queue does with a capability it was never asked about,
// what the audit log says about a call that did not happen, and what the mailbox looks like
// afterwards.

// TestHoldQueuesTheSendAndRecordsItAsHeld is the base case: nothing reaches the mailbox, the
// queue holds it, and the audit row does not claim mail went out.
func TestHoldQueuesTheSendAndRecordsItAsHeld(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapSend}, mode: grant.ModeHold,
	})

	res := s.callOK("mail_send", map[string]any{
		"account": "work",
		"to":      []map[string]any{{"email": "finance@example.net"}},
		"subject": "the invoice",
		"body":    "here it is",
	})
	if res.payload["held"] != true {
		t.Fatalf("mail_send did not report the call as held:\n%s", res.text)
	}
	if !strings.Contains(str(res.payload["approve_at"]), "/held") {
		t.Errorf("the result did not point at the queue page: %v", res.payload["approve_at"])
	}
	if got := r.mailbox(work).snapshot().sends; got != 0 {
		t.Fatalf("%d messages reached the mailbox from a held send", got)
	}

	pending := r.pending()
	if len(pending) != 1 {
		t.Fatalf("the queue holds %d actions, want 1", len(pending))
	}
	if pending[0].Kind != held.KindSend {
		t.Errorf("queued kind was %q", pending[0].Kind)
	}

	row := r.lastAuditFor("mail.send")
	if row.Outcome != "held" {
		t.Errorf("the audit row for a held send says outcome=%q; a held send marked ok reads, "+
			"months later, as mail that went out", row.Outcome)
	}
	if row.Capability != string(mail.CapSend) {
		t.Errorf("the held row records capability %q", row.Capability)
	}
	if len(row.Detail.To) != 1 || row.Detail.Subject != "the invoice" {
		t.Errorf("the held row lost what was asked for: %+v", row.Detail)
	}

	// And approving it does deliver, from the operator's own page.
	resp := r.approveHeld(pending[0].ID)
	defer resp.Body.Close()
	sent := r.mailbox(work).sentMessages()
	if len(sent) != 1 || sent[0].Subject != "the invoice" {
		t.Fatalf("approving sent %d messages: %+v", len(sent), sent)
	}
	if len(r.pending()) != 0 {
		t.Error("the action is still pending after being approved")
	}
	_ = id
}

// TestHoldRefusesOnCapabilityRatherThanQueueing settles which refusal wins.
//
// A grant in hold mode that may not do the thing at all must be refused, not queued: a queue
// of actions the grant could never perform is a queue whose entries are false — approving one
// would fail, and the page would have offered the operator a decision that was never theirs
// to make. The tool is not even registered when the capability is absent, so this drives the
// case the registration filter cannot catch: a mailbox outside the grant's scope.
func TestHoldRefusesOnCapabilityRatherThanQueueing(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	personal := r.link("personal", "ada@home.example")

	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapSend, mail.CapDestructive},
		mode: grant.ModeHold,
	})

	// A capability the grant does not hold at all: the tool is never offered.
	if names := s.toolNames(); slices.Contains(names, "mail_filters") {
		t.Errorf("a grant without `filters` was offered mail_filters: %v", names)
	}

	// A mailbox outside the grant, on a capability it does hold.
	refused := s.callError("mail_send", map[string]any{
		"account": "personal",
		"to":      []map[string]any{{"email": "x@example.net"}},
		"subject": "not yours",
	})
	if !strings.Contains(refused.text, "unknown account") && !strings.Contains(refused.text, "scope") {
		t.Errorf("the refusal did not name a scope problem:\n%s", refused.text)
	}
	if len(r.pending()) != 0 {
		t.Fatalf("a call the grant may not make was queued instead of refused: %+v", r.pending())
	}
	if got := r.mailbox(personal).snapshot().sends; got != 0 {
		t.Fatalf("%d messages reached a mailbox outside the grant", got)
	}

	// A trash naming an out-of-scope id is refused per id, and queues nothing.
	other := r.mailbox(personal).seed("theirs", "x.txt", "text/plain", []byte("x"))
	s.callError("mail_trash", map[string]any{"ids": []string{other.String()}, "action": "delete"})
	if len(r.pending()) != 0 {
		t.Fatalf("an out-of-scope delete was queued: %+v", r.pending())
	}

	// The refusal is on the record, which is the half that used to be missing entirely.
	rows := r.auditFor("mail.trash")
	if len(rows) == 0 {
		t.Fatalf("a refused mail.trash wrote no audit row:\n%s", r.auditDump())
	}
	if rows[0].Outcome == "ok" || rows[0].Outcome == "held" {
		t.Errorf("a refused trash was recorded as %q", rows[0].Outcome)
	}
}

// TestHoldDoesNotHoldDiscard confirms the deliberate hole in the queue is deliberate, and
// that it stops where it is meant to.
//
// mail_draft's own description promises "Nothing on this tool is held, deleting included".
// That is a choice — a draft is not mail anybody has received — and it is only defensible if
// the thing it lets through really is confined to unsent text. So this checks both halves:
// discard runs immediately under hold, and the destructive capability that removes received
// mail still does not.
func TestHoldDoesNotHoldDiscard(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft, mail.CapDiscard, mail.CapDestructive},
		mode: grant.ModeHold,
	})

	created := s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "a draft", "body": "text",
	})
	draftID := str(created.payload["draft_id"])
	if draftID == "" {
		t.Fatalf("no draft id came back:\n%s", created.text)
	}
	if r.mailbox(work).draftCount() != 1 {
		t.Fatal("the draft did not reach the mailbox")
	}

	deleted := s.callOK("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})
	if deleted.payload["deleted"] != draftID {
		t.Fatalf("the delete did not report the draft:\n%s", deleted.text)
	}
	if r.mailbox(work).draftCount() != 0 {
		t.Error("a discard under `hold` did not remove the draft, though the tool says it is not held")
	}
	if len(r.pending()) != 0 {
		t.Errorf("a discard was queued, contradicting mail_draft's own description: %+v", r.pending())
	}

	row := r.lastAuditFor("mail.draft")
	if row.Capability != string(mail.CapDiscard) {
		t.Errorf("the discard was recorded against capability %q, not `discard`", row.Capability)
	}
	if row.Outcome != "ok" {
		t.Errorf("the discard was recorded as %q", row.Outcome)
	}

	// The line the exemption must not cross: received mail.
	msg := r.mailbox(work).seed("arrived", "f.txt", "text/plain", []byte("f"))
	trash := s.callOK("mail_trash", map[string]any{"ids": []string{msg.String()}, "action": "delete"})
	if !strings.Contains(trash.text, "held") {
		t.Fatalf("a delete of received mail was not held:\n%s", trash.text)
	}
	if got := r.mailbox(work).snapshot().deleted; got != 0 {
		t.Fatalf("%d messages were deleted under `hold`", got)
	}
}

// TestHoldDoesNotHoldUntrash is the other documented exemption: putting mail back takes
// nothing away, so it runs.
func TestHoldDoesNotHoldUntrash(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapDestructive}, mode: grant.ModeHold,
	})
	msg := r.mailbox(work).seed("arrived", "f.txt", "text/plain", []byte("f"))

	s.callOK("mail_trash", map[string]any{"ids": []string{msg.String()}, "action": "untrash"})
	if got := r.mailbox(work).snapshot().untrashed; got != 1 {
		t.Fatalf("untrash reached the mailbox %d times under `hold`", got)
	}
	if len(r.pending()) != 0 {
		t.Errorf("untrash was queued: %+v", r.pending())
	}
}

// TestHoldModeAdvertisesToolsTheGrantDoesNotHave reports what mail_accounts tells a client
// about which of its tools are held.
//
// mail_accounts is the call every client is told to make first, and under `hold` it answers
// with a held_tools list. The list is a constant, so it names tools this grant was never
// given. That is not a security hole — an unheld tool is not a reachable one — but it is the
// one place a client is told what this connection will and will not do, and it is wrong.
func TestHoldModeAdvertisesToolsTheGrantDoesNotHave(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead}, mode: grant.ModeHold,
	})

	res := s.callOK("mail_accounts", map[string]any{})
	mode, _ := res.payload["mode"].(map[string]any)
	if mode == nil {
		t.Fatalf("mail_accounts reported no mode:\n%s", res.text)
	}
	if mode["enforced"] != true {
		t.Errorf("hold reported enforced=%v", mode["enforced"])
	}
	held, _ := mode["held_tools"].([]any)
	names := s.toolNames()
	var phantom []string
	for _, entry := range held {
		tool, _, _ := strings.Cut(str(entry), " ")
		if !slices.Contains(names, tool) {
			phantom = append(phantom, tool)
		}
	}
	if len(phantom) > 0 {
		t.Logf("FINDING (low): under `hold`, mail_accounts advertises held_tools %v, "+
			"none of which this grant holds; tools/list offers only %v", phantom, names)
	}
	if len(phantom) != len(held) {
		t.Errorf("expected every held_tool to be absent from a read-only grant, got %v of %v",
			phantom, held)
	}
}

// TestHeldSendsBypassTheSendLimitAtQueueTime measures how the per-grant send cap and the
// queue compose.
//
// The cap counts audit rows where a send came out ok, and a held send is recorded as `held`,
// so queueing does not spend the allowance. Approving does not consult the cap at all: the
// operator is the authority at that point. The consequence is that the number of messages a
// grant can put into the world in one window is the queue depth rather than the cap — every
// one of them behind a human pressing a button, which is the mitigating half.
func TestHeldSendsBypassTheSendLimitAtQueueTime(t *testing.T) {
	r := newRig(t, options{sendLimit: 1})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})

	for i := range 3 {
		res := s.callOK("mail_send", map[string]any{
			"account": "work",
			"to":      []map[string]any{{"email": "x@example.net"}},
			"subject": "message",
			"body":    "n",
		})
		if res.payload["held"] != true {
			t.Fatalf("send %d was not held (the cap is 1):\n%s", i, res.text)
		}
	}
	pending := r.pending()
	if len(pending) != 3 {
		t.Fatalf("the queue holds %d of 3 sends against a cap of 1", len(pending))
	}

	for _, a := range pending {
		resp := r.approveHeld(a.ID)
		resp.Body.Close()
	}
	sent := len(r.mailbox(work).sentMessages())
	if sent != 3 {
		t.Fatalf("approving three held sends delivered %d messages", sent)
	}
	t.Logf("FINDING (low): a grant capped at 1 send per window delivered %d, "+
		"because the cap is spent by an `ok` row and a held send writes `held`; "+
		"approval does not re-check the cap. Each delivery still needed a human press.", sent)
}

// TestComingOffHoldDoesNotReleaseTheQueue. Loosening a grant's mode must not perform what it
// queued while it was strict: those actions were held because a human had not seen them, and
// changing the setting is not having seen them. The grant edit page says so; this is the
// behaviour behind the sentence.
func TestComingOffHoldDoesNotReleaseTheQueue(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "queued while strict", "body": "x",
	})
	if len(r.pending()) != 1 {
		t.Fatalf("the queue holds %d actions", len(r.pending()))
	}

	if err := r.db.EditGrant(r.ctx, r.owner.ID, id, []mail.AccountID{work.ID},
		mail.NewSet(mail.CapSend), grant.ModeUnattended, nil); err != nil {
		t.Fatalf("loosening the mode: %v", err)
	}

	if got := len(r.mailbox(work).sentMessages()); got != 0 {
		t.Fatalf("loosening the mode delivered %d queued messages", got)
	}
	if len(r.pending()) != 1 {
		t.Fatalf("the queued action went somewhere: %+v", r.pending())
	}

	// And the grant now sends immediately, which is the other half of the change.
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "y@example.net"}},
		"subject": "after the change", "body": "x",
	})
	sent := r.mailbox(work).sentMessages()
	if len(sent) != 1 || sent[0].Subject != "after the change" {
		t.Fatalf("after loosening, the mailbox holds %+v", sent)
	}
	if len(r.pending()) != 1 {
		t.Error("the older action stopped waiting")
	}
}

// TestTheSendLimitBitesWithoutHold is the control for the held-queue finding above: outside
// `hold`, the cap does what it says.
func TestTheSendLimitBitesWithoutHold(t *testing.T) {
	r := newRig(t, options{sendLimit: 1})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Capped", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeUnattended,
	})

	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "a@example.net"}}, "subject": "one",
	})
	refused := s.callError("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "b@example.net"}}, "subject": "two",
	})
	if !strings.Contains(refused.text, "send limit reached") {
		t.Fatalf("the second send was refused with:\n%s", refused.text)
	}
	if got := len(r.mailbox(work).sentMessages()); got != 1 {
		t.Fatalf("%d messages left the mailbox against a cap of 1", got)
	}

	// The refusal is on the record with the recipients it would have reached, which is the
	// one refusal an operator is most likely to come looking for.
	var invalid store.AuditEntry
	for _, row := range r.auditFor("mail.send") {
		if row.Outcome == grant.OutcomeInvalid {
			invalid = row
		}
	}
	if invalid.Outcome != grant.OutcomeInvalid {
		t.Fatalf("the refused send wrote no row:\n%s", r.auditDump())
	}
	if len(invalid.Detail.To) != 1 || invalid.Detail.To[0] != "b@example.net" {
		t.Errorf("the refused row recorded recipients %v", invalid.Detail.To)
	}
}
