package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// Revocation crossed with the held queue.
//
// Revocation is documented everywhere else in this product as immediate and total: the token
// stops resolving, the signed links die, the consent screen has to be walked again. The
// queue is the one place that keeps something a revoked grant asked for, and it is the place
// that sends mail. These record what actually happens.

// TestApprovingSurvivesRevokingTheGrant records the current behaviour, which is deliberate
// and is a design question rather than a defect.
//
// internal/web/held.go says so in as many words: a revoked grant's action "does not stop the
// action being approved … the queue is their outbox rather than the client's", and the row
// carries a warning badge. The argument holds as far as it goes — the operator reads the
// message and presses the button, so nothing happens that a person did not choose.
//
// What this test pins down is the part that argument does not cover: the operator reached
// this state by pressing Revoke, and the page they pressed it on says "the token this client
// holds stops resolving" and "nothing else is touched", and says nothing whatever about the
// actions of that client's still waiting to be sent. See the report accompanying this commit.
func TestApprovingSurvivesRevokingTheGrant(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})

	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "finance@example.net"}},
		"subject": "queued before the revocation", "body": "x",
	})
	pending := r.pending()
	if len(pending) != 1 {
		t.Fatalf("the queue holds %d actions", len(pending))
	}

	r.revoke(id)

	// The queue still holds it, and the page marks the grant as revoked.
	afterRevoke := r.pending()
	if len(afterRevoke) != 1 {
		t.Fatalf("revoking left %d actions waiting", len(afterRevoke))
	}
	if !afterRevoke[0].GrantRevoked {
		t.Error("the waiting action does not know its grant was revoked, so the page cannot say so")
	}
	page, _ := r.page("/held")
	if !strings.Contains(strings.ToLower(page), "revoked") {
		t.Errorf("the queue page does not warn that the grant behind this action was revoked")
	}

	resp := r.approveHeld(afterRevoke[0].ID)
	defer resp.Body.Close()
	sent := r.mailbox(work).sentMessages()

	if len(sent) == 1 {
		t.Logf("FINDING (medium, design): approving a held action whose grant has been revoked "+
			"still delivered the message (%q). Revocation is total everywhere else — the token, "+
			"the signed links, the tool list — and the revoke confirmation page lists what stops "+
			"working without mentioning the queue at all.", sent[0].Subject)
	} else {
		t.Fatalf("approving after revocation delivered %d messages, which is not the behaviour "+
			"this test was written against; re-read the report", len(sent))
	}
}

// TestApprovingSurvivesRemovingTheGrant is the same question one step further on, where the
// grant is not merely revoked but gone from the page.
//
// This one is harder to defend than the revoked case. A removed grant is loaded by nothing:
// store.Grant filters it out, so the MCP endpoint, the token path and every attachment fetch
// treat it as though it never existed. The queue is the single exception, and the row it
// keeps still names a mailbox and still sends.
func TestApprovingSurvivesRemovingTheGrant(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, id := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "finance@example.net"}},
		"subject": "queued before the removal", "body": "x",
	})
	pending := r.pending()

	r.revoke(id)
	r.removeGrant(id)
	if _, err := r.db.Grant(r.ctx, id); err == nil {
		t.Fatal("the grant is still loadable, so this test is not testing removal")
	}

	still := r.pending()
	if len(still) != 1 {
		t.Fatalf("removing the grant left %d actions waiting", len(still))
	}

	resp := r.approveHeld(pending[0].ID)
	defer resp.Body.Close()
	if got := len(r.mailbox(work).sentMessages()); got == 1 {
		t.Logf("FINDING (medium, design): a held send belonging to a grant that has been " +
			"revoked and then removed was still approvable, and delivered. Nothing else in " +
			"the product will load that grant.")
	} else {
		t.Fatalf("approving after removal delivered %d messages; re-read the report", got)
	}
}

// TestDecliningAHeldActionSendsNothing is the other half, and is clean.
func TestDecliningAHeldActionSendsNothing(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "never mind", "body": "x",
	})
	pending := r.pending()

	resp := r.declineHeld(pending[0].ID)
	defer resp.Body.Close()
	if got := len(r.mailbox(work).sentMessages()); got != 0 {
		t.Fatalf("declining delivered %d messages", got)
	}
	if len(r.pending()) != 0 {
		t.Error("the declined action is still waiting")
	}
	row := r.lastAuditFor("mail.send")
	if row.Outcome != "declined" {
		t.Errorf("declining recorded outcome %q, want declined", row.Outcome)
	}
}

// TestApprovingTwiceSendsOnce. Two tabs, or a resubmitted form, race on one UPDATE.
func TestApprovingTwiceSendsOnce(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "once", "body": "x",
	})
	id := r.pending()[0].ID

	first := r.approveHeld(id)
	first.Body.Close()
	second := r.approveHeld(id)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Errorf("a second approval answered %d, want 404", second.StatusCode)
	}
	if got := len(r.mailbox(work).sentMessages()); got != 1 {
		t.Fatalf("a double submit delivered %d messages", got)
	}
}

// TestHeldActionsAreScopedToTheirOwner. A second operator must not be able to approve, or
// even learn about, what a client composed in somebody else's mailbox.
func TestHeldActionsAreScopedToTheirOwner(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "ada's mail", "body": "x",
	})
	id := r.pending()[0].ID

	// A second operator on the same instance.
	r.operator = "bob@example.com"
	page, _ := r.page("/held")
	if strings.Contains(page, "ada's mail") || strings.Contains(page, id) {
		t.Fatal("a second operator can see what a client composed in somebody else's mailbox")
	}
	resp := r.post("/held/approve", url.Values{"csrf_token": {r.csrfToken()}, "id": {id}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a second operator's approval answered %d, want 404", resp.StatusCode)
	}
	if got := len(r.mailbox(work).sentMessages()); got != 0 {
		t.Fatalf("a second operator sent %d of somebody else's messages", got)
	}
}
