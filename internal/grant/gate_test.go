package grant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// fakeResolver stands in for the store, and enforces ownership the same way it does: a
// lookup for the wrong owner reports the account as missing rather than as forbidden.
type fakeResolver struct {
	accounts map[mail.AccountID]mail.Account
}

func (f *fakeResolver) Account(_ context.Context, owner user.ID, id mail.AccountID) (mail.Account, error) {
	a, ok := f.accounts[id]
	if !ok || a.OwnerID != owner {
		return mail.Account{}, mail.ErrNotFound
	}
	return a, nil
}

func (f *fakeResolver) AccountByAlias(_ context.Context, owner user.ID, alias string) (mail.Account, error) {
	for _, a := range f.accounts {
		if a.Alias == alias && a.OwnerID == owner {
			return a, nil
		}
	}
	return mail.Account{}, mail.ErrNotFound
}

func (f *fakeResolver) AccountByAddress(_ context.Context, owner user.ID, addr string) (mail.Account, error) {
	for _, a := range f.accounts {
		if a.Address == addr && a.OwnerID == owner {
			return a, nil
		}
	}
	return mail.Account{}, mail.ErrNotFound
}

const (
	alice = user.ID("user_alice")
	bob   = user.ID("user_bob")
)

func testAccounts() *fakeResolver {
	return &fakeResolver{accounts: map[mail.AccountID]mail.Account{
		"acct_work": {ID: "acct_work", OwnerID: alice, Alias: "work", Address: "you@example.com", Provider: mail.ProviderGmail, Status: mail.StatusLinked},
		"acct_home": {ID: "acct_home", OwnerID: alice, Alias: "home", Address: "me@example.net", Provider: mail.ProviderGmail, Status: mail.StatusLinked},
		"acct_dead": {ID: "acct_dead", OwnerID: alice, Alias: "dead", Address: "old@example.org", Provider: mail.ProviderIMAP, Status: mail.StatusNeedsReauth},
		// Bob's mailbox. No grant of Alice's may ever reach it.
		"acct_bob": {ID: "acct_bob", OwnerID: bob, Alias: "bobmail", Address: "bob@example.com", Provider: mail.ProviderGmail, Status: mail.StatusLinked},
	}}
}

func readGrant(accounts ...mail.AccountID) *Grant {
	return &Grant{
		ID:       "grant_1",
		OwnerID:  alice,
		Accounts: accounts,
		Caps:     mail.NewSet(mail.CapRead, mail.CapDraft),
	}
}

func TestGateDeniesUngrantedAccount(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	_, err := g.Resolve(context.Background(), gr, "mail.search", []string{"home"}, mail.CapRead)
	if err == nil {
		t.Fatal("expected an account outside the grant to be refused")
	}
	var scope *mail.ScopeError
	if !errors.As(err, &scope) {
		t.Fatalf("want ScopeError, got %T: %v", err, err)
	}
}

// A caller that names two mailboxes and silently receives results from one will report to
// its user as though it searched both. Explicit selectors must fail loudly.
func TestGateFailsLoudlyOnPartiallyGrantedSelector(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	_, err := g.Resolve(context.Background(), gr, "mail.search", []string{"work", "home"}, mail.CapRead)
	if err == nil {
		t.Fatal("expected refusal when one of several named accounts is outside the grant")
	}
}

func TestGateOmittedSelectorMeansEverythingGranted(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work", "acct_home")

	got, err := g.Resolve(context.Background(), gr, "mail.search", nil, mail.CapRead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both granted accounts, got %d", len(got))
	}
}

func TestGateDeniesMissingCapability(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	_, err := g.Resolve(context.Background(), gr, "mail.search", []string{"work"}, mail.CapSend)
	var scope *mail.ScopeError
	if !errors.As(err, &scope) {
		t.Fatalf("want ScopeError for missing send, got %T: %v", err, err)
	}
	if scope.Capability != mail.CapSend {
		t.Fatalf("error should name the missing capability, got %q", scope.Capability)
	}
}

// An account the grant cannot see must not leak which capabilities it supports, so the
// account check has to run before the capability check.
func TestGateChecksAccountBeforeCapability(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	_, err := g.Resolve(context.Background(), gr, "mail.search", []string{"home"}, mail.CapSend)
	var scope *mail.ScopeError
	if !errors.As(err, &scope) {
		t.Fatalf("want ScopeError, got %T", err)
	}
	if scope.Capability != "" {
		t.Fatalf("an unreachable account must not report capability detail, got %q", scope.Capability)
	}
}

func TestGateRejectsRevokedAndExpired(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	now := time.Now()

	revokedAt := now.Add(-time.Hour)
	revoked := readGrant("acct_work")
	revoked.RevokedAt = &revokedAt
	if _, err := g.Resolve(context.Background(), revoked, "mail.search", nil, mail.CapRead); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked grant should be refused, got %v", err)
	}

	expiredAt := now.Add(-time.Minute)
	expired := readGrant("acct_work")
	expired.ExpiresAt = &expiredAt
	if _, err := g.Resolve(context.Background(), expired, "mail.search", nil, mail.CapRead); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired grant should be refused, got %v", err)
	}
}

func TestGateReportsReauthDistinctly(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_dead")

	_, err := g.Resolve(context.Background(), gr, "mail.search", []string{"dead"}, mail.CapRead)
	if !errors.Is(err, mail.ErrNeedsReauth) {
		t.Fatalf("want ErrNeedsReauth so the operator is told to re-link, got %v", err)
	}
}

// A grant naming an account that was later deleted keeps working for the accounts that
// remain, rather than becoming unusable.
func TestGateSkipsDeletedAccountsOnFanOut(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work", "acct_vanished")

	got, err := g.Resolve(context.Background(), gr, "mail.search", nil, mail.CapRead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Alias != "work" {
		t.Fatalf("want just the surviving account, got %+v", got)
	}
}

func TestResolveOneUsesAccountFromID(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	acct, err := g.ResolveOne(context.Background(), gr, "mail.get_message", mail.ScopedID{Account: "acct_work", Native: "abc"}, mail.CapRead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Alias != "work" {
		t.Fatalf("want work, got %s", acct.Alias)
	}

	if _, err := g.ResolveOne(context.Background(), gr, "mail.get_message", mail.ScopedID{Account: "acct_home", Native: "abc"}, mail.CapRead); err == nil {
		t.Fatal("an id belonging to an ungranted account must be refused")
	}
}

func TestEmptyCapabilitySetDeniesEverything(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := &Grant{ID: "g", Accounts: []mail.AccountID{"acct_work"}, Caps: mail.NewSet()}

	for _, c := range mail.AllCapabilities {
		if _, err := g.Resolve(context.Background(), gr, "mail.search", []string{"work"}, c); err == nil {
			t.Fatalf("empty capability set must deny %q", c)
		}
	}
}

// --- cross-user isolation ---
//
// The security property of a shared instance: a grant reaches its owner's mail and nothing
// else. These check it at the gate, which is the only path from a tool to a provider.

// A grant that names another user's mailbox — however it came to name one — must not resolve
// it. This is the backstop behind the consent screen only offering your own mailboxes.
func TestGateRefusesAccountsOwnedByAnotherUser(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_bob")

	if _, err := g.Resolve(context.Background(), gr, "mail.search", nil, mail.CapRead); err == nil {
		t.Fatal("a grant must not resolve a mailbox belonging to another user")
	}
	if _, err := g.Resolve(context.Background(), gr, "mail.search", []string{"bobmail"}, mail.CapRead); err == nil {
		t.Fatal("naming another user's mailbox by alias must be refused")
	}
	if _, err := g.Resolve(context.Background(), gr, "mail.search", []string{"bob@example.com"}, mail.CapRead); err == nil {
		t.Fatal("naming another user's mailbox by address must be refused")
	}
}

// A fan-out must not quietly pick up another user's mailboxes alongside your own.
func TestGateFanOutStaysWithinTheOwner(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work", "acct_home", "acct_bob")

	got, err := g.Resolve(context.Background(), gr, "mail.search", nil, mail.CapRead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range got {
		if a.OwnerID != alice {
			t.Errorf("fan-out returned %s, owned by %s", a.Alias, a.OwnerID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want only the two mailboxes Alice owns, got %d", len(got))
	}
}

// Guessing an id is the other way in: a message id names its account, so ResolveOne is the
// path a caller would use to reach a mailbox it was never granted.
func TestResolveOneRefusesAnotherUsersAccount(t *testing.T) {
	g := NewGate(testAccounts(), nil, nil)
	gr := readGrant("acct_work")

	_, err := g.ResolveOne(context.Background(), gr, "mail.get_message",
		mail.ScopedID{Account: "acct_bob", Native: "abc"}, mail.CapRead)
	if !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("want ErrNotFound for another user's account, got %v", err)
	}
}

// Two users may hold grants naming their own mailboxes at the same time without either
// reaching the other's.
func TestTwoUsersAreIsolatedFromEachOther(t *testing.T) {
	resolver := testAccounts()
	g := NewGate(resolver, nil, nil)

	aliceGrant := &Grant{ID: "g_alice", OwnerID: alice, Accounts: []mail.AccountID{"acct_work"}, Caps: mail.NewSet(mail.CapRead)}
	bobGrant := &Grant{ID: "g_bob", OwnerID: bob, Accounts: []mail.AccountID{"acct_bob"}, Caps: mail.NewSet(mail.CapRead)}

	got, err := g.Resolve(context.Background(), aliceGrant, "mail.search", nil, mail.CapRead)
	if err != nil || len(got) != 1 || got[0].ID != "acct_work" {
		t.Fatalf("alice should reach her own mailbox, got %+v (%v)", got, err)
	}
	got, err = g.Resolve(context.Background(), bobGrant, "mail.search", nil, mail.CapRead)
	if err != nil || len(got) != 1 || got[0].ID != "acct_bob" {
		t.Fatalf("bob should reach his own mailbox, got %+v (%v)", got, err)
	}

	// And neither can reach the other's by naming it.
	if _, err := g.Resolve(context.Background(), bobGrant, "mail.search", []string{"work"}, mail.CapRead); err == nil {
		t.Error("bob must not reach alice's mailbox by alias")
	}
	if _, err := g.Resolve(context.Background(), aliceGrant, "mail.search", []string{"bobmail"}, mail.CapRead); err == nil {
		t.Error("alice must not reach bob's mailbox by alias")
	}
}

// The audit trail records who the call belonged to, so a shared instance can answer "what
// did my grants do" without showing anyone else's activity.
func TestAuditCarriesTheOwner(t *testing.T) {
	rec := &recordingAuditor{}
	g := NewGate(testAccounts(), rec, nil)
	gr := readGrant("acct_work")

	if err := g.Record(context.Background(), gr, Audit{
		AccountID: "acct_work", Tool: "mail.search", Outcome: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.entries) != 1 || rec.entries[0].OwnerID != alice {
		t.Fatalf("audit entry should carry the owner, got %+v", rec.entries)
	}
}

type recordingAuditor struct{ entries []Audit }

func (r *recordingAuditor) Record(_ context.Context, e Audit) error {
	r.entries = append(r.entries, e)
	return nil
}

// recordingStatus captures what the gate marks, so a test can tell a write from a no-op.
type recordingStatus struct {
	calls []mail.AccountStatus
	err   error
}

func (r *recordingStatus) SetAccountStatus(_ context.Context, _ user.ID, _ mail.AccountID, s mail.AccountStatus) error {
	r.calls = append(r.calls, s)
	return r.err
}

// Every piece of the re-link pathway existed except this write: providers return
// ErrNeedsReauth, Valid refuses on the status, and the mailboxes page renders it — so a
// mailbox whose credentials had died reported that on every call and still showed as healthy
// on the page it is managed from.
func TestADeadMailboxIsMarkedForRelinking(t *testing.T) {
	status := &recordingStatus{}
	g := NewGate(testAccounts(), nil, status)
	gr := &Grant{OwnerID: "user_1", Accounts: []mail.AccountID{"acct_1"}}

	g.Observe(context.Background(), gr, "acct_1", mail.CodeAuthExpired)

	if len(status.calls) != 1 || status.calls[0] != mail.StatusNeedsReauth {
		t.Fatalf("expected the mailbox to be marked needs_reauth, got %v", status.calls)
	}
}

// Nothing else about an outcome is durable. A timeout, a refusal, a provider hiccup — none
// of them say the credentials are gone, and marking on any of them would send an operator to
// re-link a mailbox that is fine.
func TestOtherOutcomesLeaveTheMailboxAlone(t *testing.T) {
	for _, outcome := range []string{"ok", "not_found", "provider_error", "scope_denied", "timeout", "unsupported_by_provider", ""} {
		status := &recordingStatus{}
		g := NewGate(testAccounts(), nil, status)
		gr := &Grant{OwnerID: "user_1", Accounts: []mail.AccountID{"acct_1"}}

		g.Observe(context.Background(), gr, "acct_1", outcome)

		if len(status.calls) != 0 {
			t.Errorf("outcome %q must not mark the mailbox, got %v", outcome, status.calls)
		}
	}
}

// The caller is already reporting the real error. A failure to write bookkeeping must not
// displace it, and must not panic on an instance with no status writer wired.
func TestMarkingFailuresDoNotPropagate(t *testing.T) {
	gr := &Grant{OwnerID: "user_1", Accounts: []mail.AccountID{"acct_1"}}

	failing := &recordingStatus{err: errors.New("database is locked")}
	NewGate(testAccounts(), nil, failing).Observe(context.Background(), gr, "acct_1", mail.CodeAuthExpired)

	NewGate(testAccounts(), nil, nil).Observe(context.Background(), gr, "acct_1", mail.CodeAuthExpired)
}

// The string is shared between the client-facing error taxonomy and this decision, so it is
// worth pinning that they have not drifted apart.
func TestTheMarkedOutcomeIsWhatTheErrorTaxonomyProduces(t *testing.T) {
	if got := mail.Code(mail.ErrNeedsReauth); got != mail.CodeAuthExpired {
		t.Fatalf("Code(ErrNeedsReauth) is %q but the gate watches for %q", got, mail.CodeAuthExpired)
	}
}
