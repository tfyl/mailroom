package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// auditFor renders the audit page as the given user, through the real handler and the real
// store, so these are about what somebody actually sees rather than about a view struct.
func auditFor(t *testing.T, s *Server, me user.User) string {
	t.Helper()
	return renderAs(t, s.audit, "/audit", me)
}

// The one row an operator opens this page to understand. Everything about a send that is not
// the five columns is disclosed, and all of it has to be there.
func TestTheAuditPageDisclosesWhatASendDid(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)

	three := 3
	if err := db.Record(context.Background(), grant.Audit{
		OwnerID: me.ID, GrantID: id, AccountID: "acct_1",
		Tool: "mail.send", Outcome: "ok", Capability: mail.CapSend, Affected: &three,
		Detail: grant.Detail{
			To: []string{"priya@example.com"}, Cc: []string{"sam@partner.example"},
			Bcc: []string{"records@example.com"}, Subject: "Re: quarterly numbers",
			IDs: []string{"acct_1:sent_1"},
		},
		At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	body := auditFor(t, s, me)
	for _, want := range []string{
		"send", "3 recipients", "priya@example.com", "sam@partner.example",
		"records@example.com", "Re: quarterly numbers", "acct_1:sent_1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not disclose %q", want)
		}
	}
	// The detail is behind a disclosure, not on the line. The reason anybody reads this page
	// is to find the unusual row, and a table showing every fact about every call is a table
	// nobody can scan.
	if !strings.Contains(body, "<details>") {
		t.Error("the detail should be disclosed per row rather than drawn on the line")
	}
}

// The upgrade case. Nothing backfills the columns, so this is what every row already in an
// operator's database looks like — and the page has to say that rather than draw a call that
// appears to have used no capability and affected nothing.
//
// The store half of this is TestARowWrittenBeforeTheDetailColumnsReadsAsOne, which puts the
// previous version's INSERT into a real database and checks it comes back undetailed. This is
// the other half: given such a row, what a reader sees.
func TestARowFromBeforeThisChangeRendersHonestly(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	old := auditRow{
		Time: "08:31:02", Grant: "An agent", Account: "work", Tool: "mail.search",
		Outcome: "ok", Undetailed: true,
	}
	body := renderAudit(t, s, me, []auditDay{{Label: "Today", Rows: []auditRow{old}}})

	if !strings.Contains(body, "mail.search") {
		t.Fatal("an old row must still render")
	}
	if !strings.Contains(body, "Recorded before mailroom logged") {
		t.Error("an old row should say the log did not hold this, not show it as empty")
	}
	if strings.Contains(body, "none required") {
		t.Error("an old row must not claim the call needed no capability; nobody recorded one")
	}
}

// A refusal by the gate and a mailbox that failed both read as trouble in the outcome column
// and are not the same trouble: one is fixed on the grants page, the other is somebody else's
// outage. The page has to carry enough to tell them apart without leaving the page.
func TestTheAuditPageSeparatesARefusalFromAnOutage(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)
	ctx := context.Background()

	for _, e := range []grant.Audit{
		{
			OwnerID: me.ID, GrantID: id, AccountID: "acct_1", Tool: "mail.send",
			Outcome: "scope_denied", Capability: mail.CapSend,
			Reason: `scope_denied: this grant holds read on work. That action requires "send".`,
			At:     time.Now(),
		},
		{
			OwnerID: me.ID, GrantID: id, AccountID: "acct_1", Tool: "mail.get_message",
			Outcome: "provider_error", Capability: mail.CapRead,
			Reason: "provider_error: fetch on work (imap): connection reset by peer",
			At:     time.Now().Add(-time.Minute),
		},
	} {
		if err := db.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	body := auditFor(t, s, me)
	for _, want := range []string{
		"scope_denied", "That action requires", "provider_error", "connection reset by peer",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	if !strings.Contains(body, "Refused only (2)") {
		t.Error("both should count as refusals in this window")
	}
}

// Ownership, on the page rather than in the query: what one operator's browser renders holds
// nothing of another operator's.
func TestTheAuditPageShowsNobodyElsesCalls(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)
	ctx := context.Background()

	signInAs(s, "bob", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var bob user.User
	for _, u := range users {
		if u.ID != me.ID {
			bob = u
		}
	}
	if bob.ID == "" {
		t.Fatal("the second sign-in created no second user")
	}

	if err := db.Record(ctx, grant.Audit{
		OwnerID: me.ID, GrantID: id, AccountID: "acct_1", Tool: "mail.send", Outcome: "ok",
		Capability: mail.CapSend,
		Detail:     grant.Detail{To: []string{"adas-contact@example.com"}, Subject: "ada's business"},
		At:         time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	body := auditFor(t, s, bob)
	for _, leaked := range []string{"adas-contact@example.com", "ada's business", "mail.send"} {
		if strings.Contains(body, leaked) {
			t.Errorf("bob's page carries %q from ada's log", leaked)
		}
	}
	if !strings.Contains(body, "Nothing recorded yet") {
		t.Error("bob has made no calls, so his page should say so")
	}
}

// renderAudit draws the audit template over rows chosen by the test, for the states a store
// cannot produce on demand.
func renderAudit(t *testing.T, s *Server, me user.User, days []auditDay) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/audit", nil)
	r = r.WithContext(user.NewContext(r.Context(), me))
	rec := httptest.NewRecorder()
	s.render(rec, r, "audit", "Audit", "audit", map[string]any{
		"Days": days, "Refusals": 0, "Total": len(days), "Window": auditWindow,
		"OnlyRefused": false,
	})
	return rec.Body.String()
}
