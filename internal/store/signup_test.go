package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

func admission(mode signup.Mode, code string, emails, domains []string) Admission {
	return Admission{Policy: signup.NewPolicy(mode, emails, domains), InviteCode: code}
}

func join(s *Store, subject string, adm Admission) (user.User, error) {
	u, _, err := s.EnsureUser(context.Background(), user.User{
		Issuer: "https://idp.example.com", Subject: subject, Email: subject + "@example.com",
	}, adm)
	return u, err
}

// Somebody has to be able to use a freshly deployed instance, so the first sign-in is
// admitted whatever the policy says. Everybody after it is the policy's decision.
func TestFirstSignInClaimsAClosedInstance(t *testing.T) {
	s := open(t)
	closed := admission(signup.Closed, "", nil, nil)

	if _, err := join(s, "ada", closed); err != nil {
		t.Fatalf("the first sign-in must succeed on a closed instance: %v", err)
	}
	if _, err := join(s, "bob", closed); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("a second identity must be refused, got %v", err)
	}
}

// Refusal applies to identities without a row, never to one that already has one. An
// operator who closes signups must not lock out the people already using the instance.
func TestClosedDoesNotLockOutExistingUsers(t *testing.T) {
	s := open(t)
	closed := admission(signup.Closed, "", nil, nil)

	ada, err := join(s, "ada", closed)
	if err != nil {
		t.Fatal(err)
	}
	again, err := join(s, "ada", closed)
	if err != nil {
		t.Fatalf("an existing user must still sign in: %v", err)
	}
	if again.ID != ada.ID {
		t.Fatal("signing in again must resolve to the same user")
	}
}

// The empty policy is the zero value of the config struct, and it must be the restrictive
// one: a mode that failed to load should refuse, not admit.
func TestUnsetPolicyRefuses(t *testing.T) {
	s := open(t)
	if _, err := join(s, "ada", Admission{}); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "bob", Admission{}); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("an unset policy must refuse, got %v", err)
	}
}

func TestOpenAdmitsAnyone(t *testing.T) {
	s := open(t)
	adm := admission(signup.Open, "", nil, nil)

	if _, err := join(s, "ada", adm); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "stranger", adm); err != nil {
		t.Fatalf("open must admit a second identity: %v", err)
	}
}

func TestAllowlistAdmitsOnlyNamedAddresses(t *testing.T) {
	s := open(t)
	adm := admission(signup.Allowlist, "", []string{"bob@example.com"}, nil)

	if _, err := join(s, "ada", adm); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "bob", adm); err != nil {
		t.Fatalf("a listed address must be admitted: %v", err)
	}
	if _, err := join(s, "carol", adm); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("an unlisted address must be refused, got %v", err)
	}
}

func TestInviteAdmitsOnceAndOnlyOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	owner, err := join(s, "ada", admission(signup.Closed, "", nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	inv, code, err := s.CreateInvite(ctx, owner.ID, "for bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := join(s, "nobody", admission(signup.Invite, "", nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("holding no code must be refused, got %v", err)
	}
	if _, err := join(s, "guesser", admission(signup.Invite, "NOTACODE", nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("an unknown code must be refused, got %v", err)
	}

	bob, err := join(s, "bob", admission(signup.Invite, code, nil, nil))
	if err != nil {
		t.Fatalf("a valid code must be admitted: %v", err)
	}

	// The same code again must not work: an invite that admits a second person is a hole
	// the operator has no way to see.
	if _, err := join(s, "carol", admission(signup.Invite, code, nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("a spent code must be refused, got %v", err)
	}

	list, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != inv.ID {
		t.Fatalf("expected the one invite back, got %#v", list)
	}
	if list[0].RedeemedBy != bob.ID {
		t.Fatalf("the invite should record who redeemed it, got %q", list[0].RedeemedBy)
	}
	if list[0].State(time.Now()) != "redeemed" {
		t.Fatalf("state should be redeemed, got %q", list[0].State(time.Now()))
	}
}

// A refused signup must leave nothing behind. If the user row were written before the invite
// check, a failed attempt would create an account the policy meant to prevent.
func TestRefusedSignupCreatesNoUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	if _, err := join(s, "ada", admission(signup.Closed, "", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "bob", admission(signup.Closed, "", nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatal("expected a refusal")
	}

	n, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a refused signup must not create a user, count is %d", n)
	}
}

func TestRevokedAndExpiredInvitesAreRefused(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	owner, err := join(s, "ada", admission(signup.Closed, "", nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	revoked, revokedCode, err := s.CreateInvite(ctx, owner.ID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "bob", admission(signup.Invite, revokedCode, nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("a revoked code must be refused, got %v", err)
	}

	// Aged past its expiry rather than created expired: a zero or negative lifetime means
	// "never expires", so this has to reproduce the state a real invite reaches by sitting
	// unused rather than a state the constructor can produce.
	stale, staleCode, err := s.CreateInvite(ctx, owner.ID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE invites SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).Unix(), stale.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "carol", admission(signup.Invite, staleCode, nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("an expired code must be refused, got %v", err)
	}
}

// Revoking is only meaningful while an invite is unspent. Reporting success on one already
// redeemed would suggest the account it created had been undone.
func TestRevokingASpentInviteFails(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	owner, err := join(s, "ada", admission(signup.Closed, "", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	inv, code, err := s.CreateInvite(ctx, owner.ID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := join(s, "bob", admission(signup.Invite, code, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, inv.ID); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("revoking a redeemed invite should fail, got %v", err)
	}
}

func TestOwnerIsTheFirstUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	adm := admission(signup.Open, "", nil, nil)

	ada, err := join(s, "ada", adm)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := join(s, "bob", adm)
	if err != nil {
		t.Fatal(err)
	}

	if owner, err := s.IsOwner(ctx, ada.ID); err != nil || !owner {
		t.Fatalf("the first user owns the instance, got %v %v", owner, err)
	}
	if owner, err := s.IsOwner(ctx, bob.ID); err != nil || owner {
		t.Fatalf("a later user does not own the instance, got %v %v", owner, err)
	}
}

// Adoption is the way back into an instance whose original login method no longer exists.
// The account keeps its id, so everything it owns follows it onto the new identity.
func TestAdoptionMovesAnAccountOntoANewLogin(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// An account created the way a password login used to create one.
	old, _, err := s.EnsureUser(ctx, user.User{Issuer: "local", Subject: "local", Name: "operator"},
		admission(signup.Closed, "", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	link(t, s, old, "acct_legacy", "legacy")

	_, code, err := s.CreateAdoptionInvite(ctx, old.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Signups stay closed throughout: adoption adds nobody, so the policy has no say.
	moved, adoptedData, err := s.EnsureUser(ctx, user.User{
		Issuer: "https://accounts.google.com", Subject: "10293", Email: "ada@example.com", Name: "Ada",
	}, admission(signup.Closed, code, nil, nil))
	if err != nil {
		t.Fatalf("adoption should succeed on a closed instance: %v", err)
	}
	if adoptedData {
		t.Error("adoption is not the first-run data adoption and must not report it")
	}
	if moved.ID != old.ID {
		t.Fatalf("the account should keep its id: %q became %q", old.ID, moved.ID)
	}
	if moved.Issuer != "https://accounts.google.com" || moved.Subject != "10293" {
		t.Fatalf("the account should answer to the new login, got %q/%q", moved.Issuer, moved.Subject)
	}
	if _, err := s.Account(ctx, moved.ID, "acct_legacy"); err != nil {
		t.Fatalf("the mailbox should have come along: %v", err)
	}

	// Still exactly one account, and still the owner.
	if n, err := s.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("adoption must not create a second user, count is %d (%v)", n, err)
	}
	if owner, err := s.IsOwner(ctx, moved.ID); err != nil || !owner {
		t.Fatalf("the adopted account should still own the instance, got %v %v", owner, err)
	}

	// The old identity is gone, so an old session cannot resolve to it any more.
	if _, _, err := s.EnsureUser(ctx, user.User{Issuer: "local", Subject: "local"},
		admission(signup.Closed, "", nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("the replaced identity must no longer resolve, got %v", err)
	}
}

// An adoption code is spent by the login that redeems it, so a leaked link cannot hand the
// account to somebody else afterwards.
func TestAdoptionCodeIsSingleUse(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	old, _, err := s.EnsureUser(ctx, user.User{Issuer: "local", Subject: "local"},
		admission(signup.Closed, "", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, code, err := s.CreateAdoptionInvite(ctx, old.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.EnsureUser(ctx, user.User{Issuer: "https://idp.example.com", Subject: "ada"},
		admission(signup.Closed, code, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureUser(ctx, user.User{Issuer: "https://idp.example.com", Subject: "mallory"},
		admission(signup.Closed, code, nil, nil)); !errors.Is(err, ErrSignupRefused) {
		t.Fatalf("a spent adoption code must not work again, got %v", err)
	}
}
