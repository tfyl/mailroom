package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

func open(t *testing.T) *Store {
	t.Helper()
	db, err := Open("sqlite://" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// anyoneMayJoin keeps the tests below about ownership rather than about admission, which
// has tests of its own.
var anyoneMayJoin = Admission{Policy: signup.Policy{Mode: signup.Open}}

func signIn(t *testing.T, s *Store, issuer, subject string) user.User {
	t.Helper()
	u, _, err := s.EnsureUser(context.Background(), user.User{
		Issuer: issuer, Subject: subject, Email: subject + "@example.com", Name: subject,
	}, anyoneMayJoin)
	if err != nil {
		t.Fatalf("ensuring user: %v", err)
	}
	return u
}

func link(t *testing.T, s *Store, owner user.User, id, alias string) mail.Account {
	t.Helper()
	a := mail.Account{
		ID: mail.AccountID(id), Alias: alias, Address: alias + "@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	if err := s.LinkAccount(context.Background(), owner.ID, a, "sealed", "scopes"); err != nil {
		t.Fatalf("linking %s: %v", alias, err)
	}
	a.OwnerID = owner.ID
	return a
}

// The property the whole model rests on: nothing one user owns is reachable by another,
// through any query, even knowing the exact id.
func TestAccountsAreIsolatedBetweenUsers(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")

	aliceBox := link(t, s, alice, "acct_alice", "alice-work")
	link(t, s, bob, "acct_bob", "bob-work")

	t.Run("listing shows only your own", func(t *testing.T) {
		got, err := s.ListAccounts(ctx, alice.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != aliceBox.ID {
			t.Fatalf("alice should see exactly her own mailbox, got %+v", got)
		}
	})

	t.Run("fetching another user's id reports not found", func(t *testing.T) {
		if _, err := s.Account(ctx, alice.ID, "acct_bob"); !errors.Is(err, mail.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("alias and address lookups do not cross users", func(t *testing.T) {
		if _, err := s.AccountByAlias(ctx, alice.ID, "bob-work"); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("alias lookup crossed users: %v", err)
		}
		if _, err := s.AccountByAddress(ctx, alice.ID, "bob-work@example.com"); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("address lookup crossed users: %v", err)
		}
	})

	// The credential is the sensitive part: reaching one is reaching the mailbox itself.
	t.Run("credentials are not readable across users", func(t *testing.T) {
		if _, err := s.Credential(ctx, alice.ID, "acct_bob"); !errors.Is(err, mail.ErrNotFound) {
			t.Fatalf("want ErrNotFound reading another user's credential, got %v", err)
		}
	})

	// A write that matched nothing must not report success, or the UI claims to have done
	// something it did not.
	t.Run("mutations across users fail rather than silently doing nothing", func(t *testing.T) {
		if err := s.UnlinkAccount(ctx, alice.ID, "acct_bob"); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("unlink crossed users or reported success: %v", err)
		}
		if err := s.RenameAccount(ctx, alice.ID, "acct_bob", "stolen"); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("rename crossed users or reported success: %v", err)
		}
		if err := s.SetAccountStatus(ctx, alice.ID, "acct_bob", mail.StatusDisabled); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("status change crossed users or reported success: %v", err)
		}

		// And Bob's mailbox is untouched by all of that.
		still, err := s.Account(ctx, bob.ID, "acct_bob")
		if err != nil || still.Alias != "bob-work" || still.Status != mail.StatusLinked {
			t.Fatalf("bob's mailbox was modified: %+v (%v)", still, err)
		}
	})
}

func TestGrantsAreIsolatedBetweenUsers(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")
	link(t, s, alice, "acct_alice", "alice-work")
	link(t, s, bob, "acct_bob", "bob-work")

	if err := s.RegisterClient(ctx, Client{ID: "client_1", Name: "c", RedirectURIs: []string{"https://x/cb"}}); err != nil {
		t.Fatal(err)
	}

	bobGrant := &grant.Grant{
		ID: "grant_bob", OwnerID: bob.ID, ClientID: "client_1", Label: "bob's",
		Accounts: []mail.AccountID{"acct_bob"}, Caps: mail.NewSet(mail.CapRead),
	}
	if err := s.CreateGrant(ctx, bobGrant); err != nil {
		t.Fatal(err)
	}

	t.Run("listing shows only your own", func(t *testing.T) {
		got, err := s.ListGrants(ctx, alice.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("alice should see no grants, got %d", len(got))
		}
	})

	// Guessing a grant id must not be enough to revoke it: that would be a denial-of-service
	// against another user's agents.
	t.Run("revoking another user's grant is refused", func(t *testing.T) {
		if err := s.RevokeGrant(ctx, alice.ID, "grant_bob"); !errors.Is(err, grant.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		g, err := s.Grant(ctx, "grant_bob")
		if err != nil {
			t.Fatal(err)
		}
		if g.Revoked() {
			t.Fatal("alice revoked bob's grant")
		}
	})

	// A grant may only name mailboxes its owner owns. The consent screen enforces this too,
	// but a grant outlives the screen that made it.
	t.Run("a grant cannot name another user's mailbox", func(t *testing.T) {
		err := s.CreateGrant(ctx, &grant.Grant{
			ID: "grant_theft", OwnerID: alice.ID, ClientID: "client_1", Label: "nope",
			Accounts: []mail.AccountID{"acct_bob"}, Caps: mail.NewSet(mail.CapRead),
		})
		if err == nil {
			t.Fatal("creating a grant over another user's mailbox must be refused")
		}
	})

	t.Run("a grant must have an owner", func(t *testing.T) {
		err := s.CreateGrant(ctx, &grant.Grant{
			ID: "grant_orphan", ClientID: "client_1", Label: "no owner",
			Accounts: []mail.AccountID{"acct_alice"}, Caps: mail.NewSet(mail.CapRead),
		})
		if err == nil {
			t.Fatal("an ownerless grant must be refused")
		}
	})
}

func TestAuditIsScopedToTheOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")

	for _, e := range []grant.Audit{
		{OwnerID: alice.ID, GrantID: "g_a", AccountID: "acct_alice", Tool: "mail.search", Outcome: "ok"},
		{OwnerID: bob.ID, GrantID: "g_b", AccountID: "acct_bob", Tool: "mail.send", Outcome: "ok"},
	} {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentAudit(ctx, alice.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tool != "mail.search" {
		t.Fatalf("alice should see only her own activity, got %+v", got)
	}
}

// Identity is keyed on issuer and subject together. Two issuers handing out the same subject
// are two different people, and treating them as one would hand over a mailbox.
func TestSameSubjectAtDifferentIssuersAreDifferentUsers(t *testing.T) {
	s := open(t)

	a := signIn(t, s, "https://idp-one.example.com", "shared-subject")
	b := signIn(t, s, "https://idp-two.example.com", "shared-subject")

	if a.ID == b.ID {
		t.Fatal("the same subject at two issuers must not resolve to one user")
	}
}

func TestSigningInTwiceReusesTheSameUser(t *testing.T) {
	s := open(t)

	first := signIn(t, s, "https://idp.example.com", "alice")
	second := signIn(t, s, "https://idp.example.com", "alice")

	if first.ID != second.ID {
		t.Fatalf("signing in again created a second user: %s then %s", first.ID, second.ID)
	}
}

// An instance upgraded from before multi-user support has mailboxes with no owner. They are
// adopted by the first person to sign in — once, and never by a second.
func TestUnownedDataIsAdoptedByTheFirstUserOnly(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// Simulate the old shape: an account row with no owner.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, alias, address, provider, status, credential, scopes, linked_at, synced_at)
		VALUES ('acct_legacy', 'legacy', 'legacy@example.com', 'gmail', 'linked', 'sealed', '', 1, 0)`); err != nil {
		t.Fatal(err)
	}

	alice, adopted, err := s.EnsureUser(ctx, user.User{Issuer: "local", Subject: "local"}, anyoneMayJoin)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("the first user should have adopted the unowned mailbox")
	}
	if _, err := s.Account(ctx, alice.ID, "acct_legacy"); err != nil {
		t.Fatalf("the adopted mailbox should belong to the first user: %v", err)
	}

	bob, adoptedAgain, err := s.EnsureUser(ctx, user.User{Issuer: "https://idp.example.com", Subject: "bob"}, anyoneMayJoin)
	if err != nil {
		t.Fatal(err)
	}
	if adoptedAgain {
		t.Fatal("a second user must not adopt anything")
	}
	if _, err := s.Account(ctx, bob.ID, "acct_legacy"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("the second user must not reach the adopted mailbox, got %v", err)
	}
}

// Adoption only makes sense for data that predates users. A mailbox linked normally is
// already owned, so a later signup must not sweep it up.
func TestAdoptionDoesNotClaimAlreadyOwnedData(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "local", "local")
	link(t, s, alice, "acct_alice", "alice-work")

	bob, adopted, err := s.EnsureUser(ctx, user.User{Issuer: "https://idp.example.com", Subject: "bob"}, anyoneMayJoin)
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Fatal("there was nothing unowned to adopt")
	}
	if _, err := s.Account(ctx, bob.ID, "acct_alice"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("bob must not have picked up alice's mailbox, got %v", err)
	}
}

// UpdateCredential is how a rotated refresh token gets written back, so it carries the same
// ownership rule as everything else here: naming another user's mailbox reports not found
// rather than overwriting the credential of a mailbox the caller cannot see.
func TestCredentialsAreRewrittenOnlyByTheirOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "https://idp.example.com", "alice")
	bob := signIn(t, s, "https://idp.example.com", "bob")
	box := link(t, s, alice, "acct_alice", "alice-work")

	if err := s.UpdateCredential(ctx, bob.ID, box.ID, "sealed-by-bob"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	got, err := s.Credential(ctx, alice.ID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sealed" {
		t.Fatalf("the credential was changed by somebody who does not own the mailbox: %q", got)
	}

	if err := s.UpdateCredential(ctx, alice.ID, box.ID, "sealed-again"); err != nil {
		t.Fatalf("the owner could not rewrite it: %v", err)
	}
	if got, err = s.Credential(ctx, alice.ID, box.ID); err != nil || got != "sealed-again" {
		t.Fatalf("want the rewritten credential, got %q (%v)", got, err)
	}
}

// The mailboxes page has always had a "used" line and it never rendered, because the column
// behind it was written by nothing. It now comes from the audit log, which already records
// every tool call against every mailbox — so the fact is stored once rather than twice, and
// on a server that proxies rather than syncs, not written on every call at all.
func TestLastUsedComesFromTheAuditLog(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "local", "local")
	acct := link(t, s, alice, "acct_used", "used")

	before, err := s.ListAccounts(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !before[0].LastUsedAt.IsZero() {
		t.Fatalf("a mailbox nothing has touched has no last use, got %v", before[0].LastUsedAt)
	}

	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := s.Record(ctx, grant.Audit{
		OwnerID: alice.ID, AccountID: acct.ID, Tool: "mail.search", Outcome: "ok", At: when,
	}); err != nil {
		t.Fatal(err)
	}

	after, err := s.ListAccounts(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].LastUsedAt.Equal(when.UTC()) {
		t.Fatalf("want last use %v, got %v", when.UTC(), after[0].LastUsedAt)
	}
}

// Another user's activity against their own mailbox must not appear on yours, and the join
// is the kind of place that boundary is easy to lose.
func TestLastUsedIsScopedToItsOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	alice := signIn(t, s, "local", "local")
	acct := link(t, s, alice, "acct_a", "alice-box")
	bob := signIn(t, s, "https://idp.example.com", "bob")

	if err := s.Record(ctx, grant.Audit{
		OwnerID: bob.ID, AccountID: acct.ID, Tool: "mail.search", Outcome: "ok", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	accounts, err := s.ListAccounts(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !accounts[0].LastUsedAt.IsZero() {
		t.Fatalf("another owner's audit row must not count as use, got %v", accounts[0].LastUsedAt)
	}
}
