package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Removing a grant is a soft delete, and these are the tests that say what that has to mean:
// gone from every read, taking its tokens and its attachments with it, and still named on
// every audit row it wrote. The last one is the whole reason the row survives, so it is
// asserted through RecentAudit rather than by looking at the table.

type removeRig struct {
	t     *testing.T
	s     *Store
	alice user.User
	bob   user.User
}

func newRemoveRig(t *testing.T) removeRig {
	t.Helper()
	s := open(t)
	ctx := context.Background()

	rig := removeRig{t: t, s: s}
	rig.alice = signIn(t, s, "https://idp.example.com", "alice")
	rig.bob = signIn(t, s, "https://idp.example.com", "bob")
	link(t, s, rig.alice, "acct_alice", "alice-work")
	link(t, s, rig.bob, "acct_bob", "bob-work")

	if err := s.RegisterClient(ctx, Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	return rig
}

// grantFor records a grant with a token against it, because what a removal has to take with
// it is exactly what a live grant leaves lying around.
func (r removeRig) grantFor(owner user.User, id, label string) (grant.ID, string) {
	r.t.Helper()
	ctx := context.Background()

	g := &grant.Grant{
		ID: grant.ID(id), OwnerID: owner.ID, ClientID: "client_1", Label: label,
		Accounts: []mail.AccountID{mail.AccountID("acct_" + owner.Subject)},
		Caps:     mail.NewSet(mail.CapRead, mail.CapAttachments),
	}
	if err := r.s.CreateGrant(ctx, g); err != nil {
		r.t.Fatal(err)
	}
	token := "token-for-" + id
	if err := r.s.IssueToken(ctx, token, g.ID, nil); err != nil {
		r.t.Fatal(err)
	}
	return g.ID, token
}

func (r removeRig) revoke(owner user.User, id grant.ID) {
	r.t.Helper()
	if err := r.s.RevokeGrant(context.Background(), owner.ID, id); err != nil {
		r.t.Fatalf("revoking %s: %v", id, err)
	}
}

func (r removeRig) tokenRows(id grant.ID) int {
	r.t.Helper()
	var n int
	if err := r.s.db.QueryRow(`SELECT COUNT(*) FROM tokens WHERE grant_id = ?`, string(id)).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r removeRig) grantRows(id grant.ID) int {
	r.t.Helper()
	var n int
	if err := r.s.db.QueryRow(`SELECT COUNT(*) FROM grants WHERE id = ?`, string(id)).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestARevokedGrantCanBeRemovedAndStopsBeingFound(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	id, token := r.grantFor(r.alice, "grant_1", "An agent")
	r.revoke(r.alice, id)

	if err := r.s.RemoveGrant(ctx, r.alice.ID, id); err != nil {
		t.Fatalf("removing a revoked grant: %v", err)
	}

	list, err := r.s.ListGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a removed grant should be off the list, got %+v", list)
	}
	if _, err := r.s.Grant(ctx, id); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("loading a removed grant by id should be a miss, got %v", err)
	}
	// Both halves of the token path: the row is gone, and the lookup that would have used it
	// no longer resolves. Either alone would leave the other believable.
	if n := r.tokenRows(id); n != 0 {
		t.Errorf("a removed grant should take its tokens with it, %d left", n)
	}
	if _, err := r.s.GrantForToken(ctx, token); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("a token for a removed grant should not resolve, got %v", err)
	}
	// Soft, and this is what soft means: the row is still there to be joined onto.
	if n := r.grantRows(id); n != 1 {
		t.Errorf("the row should survive so the audit log can still name it, got %d", n)
	}
	if err := r.s.RemoveGrant(ctx, r.alice.ID, id); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("removing the same grant twice should be a miss, got %v", err)
	}
}

// The point of the soft delete. A hard delete would keep every one of these rows and blank
// the name on all of them, which is the audit page surviving and becoming useless.
func TestTheAuditTrailStillNamesARemovedGrant(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	id, _ := r.grantFor(r.alice, "grant_1", "Nightly digest")
	for _, e := range []grant.Audit{
		{OwnerID: r.alice.ID, GrantID: id, AccountID: "acct_alice", Tool: "mail.search", Outcome: "ok"},
		{OwnerID: r.alice.ID, GrantID: id, AccountID: "acct_alice", Tool: "mail.send", Outcome: "scope_denied"},
	} {
		if err := r.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	r.revoke(r.alice, id)
	if err := r.s.RemoveGrant(ctx, r.alice.ID, id); err != nil {
		t.Fatal(err)
	}

	rows, err := r.s.RecentAudit(ctx, r.alice.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want both audit rows after the removal, got %d", len(rows))
	}
	for _, row := range rows {
		if row.GrantName != "Nightly digest" {
			t.Errorf("the audit row for %s lost its grant name: %+v", row.Tool, row)
		}
		if row.GrantID != string(id) {
			t.Errorf("the audit row lost its grant id: %+v", row)
		}
	}
}

// Revoking is the step that ends access, and it is the one that asks. A remove that could
// skip it would be a way to drop a client without ever seeing the page explaining what that
// breaks — so being revoked is the predicate, not a check somewhere upstream.
func TestOnlyARevokedGrantCanBeRemoved(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour)
	live, _ := r.grantFor(r.alice, "grant_live", "Live")
	expired, _ := r.grantFor(r.alice, "grant_expired", "Expired")
	if err := r.s.EditGrant(ctx, r.alice.ID, expired,
		[]mail.AccountID{"acct_alice"}, mail.NewSet(mail.CapRead), grant.DefaultMode, &past); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   grant.ID
	}{
		{"a live grant", live},
		// Expired is not revoked. It reaches nothing today and is one edit — a new expiry —
		// from working again, which is why the page still shows it whole and still lets it be
		// edited. Removing one would end that without asking.
		{"an expired grant", expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.s.RemoveGrant(ctx, r.alice.ID, tc.id); !errors.Is(err, grant.ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
			if _, err := r.s.Grant(ctx, tc.id); err != nil {
				t.Fatalf("the grant should be untouched, got %v", err)
			}
		})
	}

	// And the bulk path reaches no further than the single one.
	n, err := r.s.RemoveRevokedGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("clearing with nothing revoked should remove nothing, got %d", n)
	}
	list, err := r.s.ListGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("both grants should still be listed, got %d", len(list))
	}
}

// Knowing another user's grant id must buy nothing, including the knowledge that it is real.
func TestRemovingAnotherUsersGrantIsRefused(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	id, _ := r.grantFor(r.bob, "grant_bob", "Bob's agent")
	r.revoke(r.bob, id)

	if err := r.s.RemoveGrant(ctx, r.alice.ID, id); !errors.Is(err, grant.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// The same error a missing id gets, so the two cannot be told apart.
	if err := r.s.RemoveGrant(ctx, r.alice.ID, "grant_that_never_existed"); !errors.Is(err, grant.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unknown id, got %v", err)
	}

	if _, err := r.s.Grant(ctx, id); err != nil {
		t.Fatalf("bob's grant should be untouched, got %v", err)
	}
	if n := r.tokenRows(id); n != 1 {
		t.Errorf("bob's token should be untouched, got %d rows", n)
	}

	// Clearing the band clears the operator's own band and nobody else's.
	aliceGrant, _ := r.grantFor(r.alice, "grant_alice", "Alice's agent")
	r.revoke(r.alice, aliceGrant)
	n, err := r.s.RemoveRevokedGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("alice should have cleared exactly her own revoked grant, got %d", n)
	}
	if _, err := r.s.Grant(ctx, id); err != nil {
		t.Fatalf("bob's grant should have survived alice clearing hers, got %v", err)
	}
}

func TestClearingTheBandRemovesEveryRevokedGrantAtOnce(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	live, _ := r.grantFor(r.alice, "grant_live", "Live")
	for _, id := range []string{"grant_1", "grant_2", "grant_3"} {
		g, _ := r.grantFor(r.alice, id, "Revoked "+id)
		r.revoke(r.alice, g)
	}

	n, err := r.s.RemoveRevokedGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 grants cleared, got %d", n)
	}
	list, err := r.s.ListGrants(ctx, r.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != live {
		t.Fatalf("only the live grant should be left, got %+v", list)
	}
	for _, id := range []grant.ID{"grant_1", "grant_2", "grant_3"} {
		if n := r.tokenRows(id); n != 0 {
			t.Errorf("%s kept %d tokens", id, n)
		}
	}
}

// A blob names the grant that minted it and every fetch re-reads that grant, so removing one
// has already killed its links. What the removal has to do as well is stop the rows and their
// bytes sitting there until their own expiry: they are handed to the sweeper instead, which
// is the only thing that can delete both halves in the right order.
func TestRemovingAGrantHandsItsAttachmentsToTheSweeper(t *testing.T) {
	r := newRemoveRig(t)
	ctx := context.Background()

	id, _ := r.grantFor(r.alice, "grant_1", "An agent")
	ref := blob.Ref{
		ID: "blob_1", Owner: r.alice.ID, GrantID: id, Kind: blob.KindMail,
		State: blob.StateReady, AccountID: "acct_alice", Filename: "invoice.pdf",
		MimeType: "application/pdf", Size: 12,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(12 * time.Hour),
	}
	if err := r.s.PutBlob(ctx, r.alice.ID, ref); err != nil {
		t.Fatal(err)
	}

	if expired, err := r.s.ExpiredBlobs(ctx, time.Now()); err != nil || len(expired) != 0 {
		t.Fatalf("the blob should not be sweepable yet: %+v (%v)", expired, err)
	}

	r.revoke(r.alice, id)
	if err := r.s.RemoveGrant(ctx, r.alice.ID, id); err != nil {
		t.Fatal(err)
	}

	expired, err := r.s.ExpiredBlobs(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != "blob_1" {
		t.Fatalf("the removed grant's blob should be waiting for the sweeper, got %+v", expired)
	}
}
