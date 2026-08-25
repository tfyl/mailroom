package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// The detail columns arrived after the first release, so the two things worth holding are that
// what a tool records comes back the way it was written, and that a row written before any of
// it existed still reads as itself rather than as a call that recorded nothing.

func TestAuditDetailSurvivesTheRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")
	work := link(t, s, alice, "acct_alice", "work")

	three := 3
	err := s.Record(ctx, grant.Audit{
		OwnerID: alice.ID, GrantID: "g_a", AccountID: work.ID,
		Tool: "mail.send", Outcome: "ok", Capability: mail.CapSend, Affected: &three,
		Detail: grant.Detail{
			To: []string{"priya@example.com"}, Cc: []string{"sam@partner.example"},
			Bcc: []string{"records@example.com"}, Subject: "Re: quarterly numbers",
			IDs: []string{"acct_alice:sent_1"},
		},
		At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentAudit(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one row, got %d", len(got))
	}
	e := got[0]
	if !e.Detailed {
		t.Error("a row this version wrote is a detailed row")
	}
	if e.Capability != string(mail.CapSend) {
		t.Errorf("capability came back as %q", e.Capability)
	}
	if e.Affected == nil || *e.Affected != 3 {
		t.Errorf("the count came back as %v", e.Affected)
	}
	if e.Detail.Subject != "Re: quarterly numbers" || len(e.Detail.To) != 1 || len(e.Detail.Bcc) != 1 {
		t.Errorf("the detail came back as %+v", e.Detail)
	}
}

// A count of zero and no count at all are different answers, and JSON's zero value would make
// them the same one. A search that matched nothing is a fact; a tool that counts nothing is not.
func TestAnUncountedCallIsNotACountOfZero(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")

	none := 0
	for _, e := range []grant.Audit{
		{OwnerID: alice.ID, GrantID: "g_a", Tool: "mail.search", Outcome: "ok", Affected: &none, At: time.Now()},
		{OwnerID: alice.ID, GrantID: "g_a", Tool: "mail.accounts", Outcome: "ok", At: time.Now().Add(-time.Second)},
	} {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentAudit(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want two rows, got %d", len(got))
	}
	if got[0].Affected == nil || *got[0].Affected != 0 {
		t.Errorf("a search that matched nothing counted zero, got %v", got[0].Affected)
	}
	if got[1].Affected != nil {
		t.Errorf("a call that counts nothing has no count, got %v", *got[1].Affected)
	}
}

// One call must not be able to write a row the size of a mailbox. The cap is applied by the
// write rather than by the twenty places that build an entry, so this is where it is checked.
func TestOneRowCannotGrowWithoutBound(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "acct_alice:m" + strings.Repeat("9", 400)
	}
	if err := s.Record(ctx, grant.Audit{
		OwnerID: alice.ID, GrantID: "g_a", Tool: "mail.trash", Outcome: "ok",
		Reason: strings.Repeat("x", 5000),
		Detail: grant.Detail{IDs: ids, Subject: strings.Repeat("y", 5000)},
		At:     time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentAudit(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	e := got[0]
	if len(e.Detail.IDs) > 10 {
		t.Errorf("the id list is not bounded: %d entries", len(e.Detail.IDs))
	}
	if e.Detail.More != 240 {
		t.Errorf("the row should say how many it dropped, got %d", e.Detail.More)
	}
	for _, id := range e.Detail.IDs {
		if len(id) > 210 {
			t.Errorf("an individual value is not bounded: %d bytes", len(id))
		}
	}
	if len(e.Reason) > 310 || len(e.Detail.Subject) > 210 {
		t.Errorf("free text is not bounded: reason %d, subject %d", len(e.Reason), len(e.Detail.Subject))
	}
}

// The upgrade case. Nothing backfills the detail columns, so every row already in a database
// has NULL in all of them — and the page has to be able to say that rather than drawing a call
// that appears to have affected nothing.
func TestARowWrittenBeforeTheDetailColumnsReadsAsOne(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")
	work := link(t, s, alice, "acct_alice", "work")

	// Exactly the INSERT the previous version ran.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (owner_id, grant_id, account_id, tool, outcome, at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		string(alice.ID), "g_a", string(work.ID), "mail.search", "ok", unix(time.Now())); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentAudit(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one row, got %d", len(got))
	}
	e := got[0]
	if e.Detailed {
		t.Error("a row from before the detail columns must not present as a detailed one")
	}
	if e.Tool != "mail.search" || e.Outcome != "ok" || e.Account != "work" {
		t.Errorf("everything the old row did hold must still read: %+v", e)
	}
	if e.Affected != nil || e.Capability != "" || !e.Detail.Empty() {
		t.Errorf("nothing may be invented for it: %+v", e)
	}
}

// A refusal is recorded with the mailbox it was refused against, and a caller chooses part of
// the id it names. The account join that puts an alias on the page is therefore scoped to the
// row's own owner as well as to the id, or a colliding id would render another user's alias.
func TestAnAuditRowNeverRendersAnotherUsersAlias(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")
	bobsMailbox := link(t, s, bob, "acct_bob", "bobs-private-mailbox")

	if err := s.Record(ctx, grant.Audit{
		OwnerID: alice.ID, GrantID: "g_a", AccountID: bobsMailbox.ID,
		Tool: "mail.get_message", Outcome: "not_found", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentAudit(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one row, got %d", len(got))
	}
	if got[0].Account == bobsMailbox.Alias {
		t.Fatal("alice's page named bob's mailbox")
	}
	if got[0].Account != string(bobsMailbox.ID) {
		t.Errorf("an id that names nothing of this owner's stays an id, got %q", got[0].Account)
	}
}

// The property the whole model rests on, restated for the columns this change added: a detail
// belongs to the row's owner and to nobody else.
func TestAuditDetailIsScopedToTheOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")

	if err := s.Record(ctx, grant.Audit{
		OwnerID: bob.ID, GrantID: "g_b", Tool: "mail.send", Outcome: "ok",
		Capability: mail.CapSend,
		Detail:     grant.Detail{To: []string{"bobs-contact@example.com"}, Subject: "bob's business"},
		At:         time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentAudit(ctx, alice.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("alice must see nothing of bob's, got %+v", got)
	}
}
