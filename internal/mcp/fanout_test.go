package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// errUpstream stands in for whatever a provider failed with, kept plain so it classifies as
// a provider error rather than as something the taxonomy recognises more specifically.
var errUpstream = errors.New("connection reset by peer")

// Three mailboxes: two the grant names, and one it does not. The third is what makes a
// scope refusal inside a batch different from an id that names nothing at all.
var (
	workMailbox = mail.Account{
		ID: "acct_1", OwnerID: "u1", Alias: "work", Address: "work@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	archiveMailbox = mail.Account{
		ID: "acct_2", OwnerID: "u1", Alias: "archive", Address: "archive@example.net",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	ungrantedMailbox = mail.Account{
		ID: "acct_3", OwnerID: "u1", Alias: "personal", Address: "personal@example.org",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
)

type severalMailboxes struct{}

func (severalMailboxes) all() []mail.Account {
	return []mail.Account{workMailbox, archiveMailbox, ungrantedMailbox}
}

func (s severalMailboxes) Account(_ context.Context, _ user.ID, id mail.AccountID) (mail.Account, error) {
	for _, a := range s.all() {
		if a.ID == id {
			return a, nil
		}
	}
	return mail.Account{}, mail.ErrNotFound
}

func (s severalMailboxes) AccountByAlias(_ context.Context, _ user.ID, alias string) (mail.Account, error) {
	for _, a := range s.all() {
		if a.Alias == alias {
			return a, nil
		}
	}
	return mail.Account{}, mail.ErrNotFound
}

func (s severalMailboxes) AccountByAddress(_ context.Context, _ user.ID, address string) (mail.Account, error) {
	for _, a := range s.all() {
		if a.Address == address {
			return a, nil
		}
	}
	return mail.Account{}, mail.ErrNotFound
}

// byAccount hands each mailbox its own provider, so one can fail while another works.
type byAccount map[mail.AccountID]mail.Provider

func (b byAccount) For(_ context.Context, acct mail.Account) (mail.Provider, error) {
	p, ok := b[acct.ID]
	if !ok {
		return nil, mail.ErrNotFound
	}
	return p, nil
}

func fanoutTools(providers byAccount) *Tools {
	return NewTools(grant.NewGate(severalMailboxes{}, silentAudit{}, nil), providers, severalMailboxes{})
}

// grantOver builds a grant over work and archive, deliberately not over personal.
func grantOver(caps ...mail.Capability) context.Context {
	return context.WithValue(context.Background(), grantKey{}, &grant.Grant{
		ID: "g1", OwnerID: "u1",
		Accounts: []mail.AccountID{workMailbox.ID, archiveMailbox.ID},
		Caps:     mail.NewSet(caps...),
	})
}

// stubLabels is a mailbox that can label, and fails every write when told to.
type stubLabels struct {
	// deleted records what actually reached the provider, which is the only way to tell a
	// refusal from a deletion that was reported as one.
	deleted []mail.LabelID
	err     error
	applied []mail.ScopedID
	labels  []mail.Label
}

func (s *stubLabels) ID() mail.ProviderID    { return mail.ProviderGmail }
func (s *stubLabels) Capabilities() mail.Set { return mail.DerivedCapabilities(s) }
func (s *stubLabels) Quirks() []mail.Quirk   { return nil }

func (s *stubLabels) ListLabels(context.Context) ([]mail.Label, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.labels, nil
}

func (s *stubLabels) CreateLabel(_ context.Context, name string, _ bool) (mail.Label, error) {
	if s.err != nil {
		return mail.Label{}, s.err
	}
	return mail.Label{ID: mail.LabelID("Label_" + name), Name: name}, nil
}

func (s *stubLabels) DeleteLabel(_ context.Context, id mail.LabelID) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, id)
	return nil
}

// DeletingDestroysMail answers the way the folder providers do: a container takes its mail
// with it, a tag does not.
func (s *stubLabels) DeletingDestroysMail(_ context.Context, id mail.LabelID) (bool, error) {
	return strings.HasPrefix(string(id), "folder:"), nil
}

// EffectOfApplying answers the way the providers do: an id that names the bin or junk is a
// trashing rather than a filing, whichever provider is speaking.
func (s *stubLabels) EffectOfApplying(_ context.Context, id mail.LabelID) (mail.LabelEffect, error) {
	return mail.EffectOfMailboxName(string(id)), nil
}

func (s *stubLabels) ApplyLabels(_ context.Context, ids []mail.ScopedID, _, _ []mail.LabelID) error {
	if s.err != nil {
		return s.err
	}
	s.applied = append(s.applied, ids...)
	return nil
}

func (s *stubLabels) SetFlags(context.Context, []mail.ScopedID, mail.FlagUpdate) error { return s.err }

// stubDestroyer can trash. Its embedded stubLabels keeps mail_modify usable against the same
// mailbox, and the failure switch works the same way.
type stubDestroyer struct{ stubLabels }

func (s *stubDestroyer) Trash(_ context.Context, ids []mail.ScopedID) error {
	if s.err != nil {
		return s.err
	}
	s.applied = append(s.applied, ids...)
	return nil
}

func (s *stubDestroyer) Untrash(context.Context, []mail.ScopedID) error { return s.err }
func (s *stubDestroyer) Delete(context.Context, []mail.ScopedID) error  { return s.err }

func accountsBlock(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	out, ok := body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("the result carries no accounts block: %v", body)
	}
	return out
}

func entry(t *testing.T, accounts map[string]any, alias string) map[string]any {
	t.Helper()
	out, ok := accounts[alias].(map[string]any)
	if !ok {
		t.Fatalf("no entry for %s: %v", alias, accounts)
	}
	return out
}

// One mistyped id used to lose the whole batch, which is the opposite of what the tool
// contract promises. The mailbox that could be modified is modified, and every id that
// reached no mailbox comes back named, with the code that says why.
func TestModifyKeepsTheWorkItCouldDo(t *testing.T) {
	work := &stubLabels{}
	archive := &stubLabels{err: &mail.ProviderError{
		Provider: mail.ProviderIMAP, Account: "archive", Op: "store", Retryable: true,
		Err: errUpstream,
	}}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: archive})

	res, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{
			"acct_1:m1", "acct_1:m2", // fine
			"acct_2:m3", // the mailbox refuses
			"not-an-id", // unparseable
			"acct_9:m4", // no such mailbox
			"acct_3:m5", // a real mailbox this grant does not name
		},
		AddLabels: []string{"Label_1"},
	})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if res.IsError {
		t.Fatalf("a batch with one good mailbox must not fail: %v", payload(t, res))
	}

	body := payload(t, res)
	accounts := accountsBlock(t, body)
	if got := entry(t, accounts, "work")["modified"]; got != float64(2) {
		t.Errorf("work should report 2 modified, got %v", got)
	}
	if len(work.applied) != 2 {
		t.Errorf("the healthy mailbox should have been asked to modify 2 messages, got %d", len(work.applied))
	}
	if got := entry(t, accounts, "archive")["error"]; got != "provider_error" {
		t.Errorf("a failing mailbox must carry a code, got %v", got)
	}

	rejected, ok := body["rejected"].([]any)
	if !ok || len(rejected) != 3 {
		t.Fatalf("want the three unroutable ids reported, got %v", body["rejected"])
	}
	codes := map[string]string{}
	for _, r := range rejected {
		row := r.(map[string]any)
		codes[row["id"].(string)] = row["error"].(string)
	}
	if codes["acct_9:m4"] != "not_found" {
		t.Errorf("an id naming no mailbox should be not_found, got %q", codes["acct_9:m4"])
	}
	if codes["acct_3:m5"] != "scope_denied" {
		t.Errorf("an id outside the grant should be scope_denied, got %q", codes["acct_3:m5"])
	}
	if _, seen := codes["not-an-id"]; !seen {
		t.Errorf("a malformed id should be reported against itself, got %v", codes)
	}
}

// A mailbox the grant does not name must still be refused, and refused for every id in the
// batch. Continuing past a bad id is about not losing the good ones, not about letting one
// through.
func TestModifyStillRefusesEveryUnauthorizedID(t *testing.T) {
	personal := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: &stubLabels{}, ungrantedMailbox.ID: personal})

	res, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1", "acct_3:m2"}, AddLabels: []string{"Label_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the authorized half of the batch should still have run: %v", payload(t, res))
	}
	if len(personal.applied) != 0 {
		t.Error("a mailbox outside the grant must not be touched")
	}
}

// Nothing succeeded, so this is a failure rather than an empty success — the same rule an
// aggregated search follows, and for the same reason: an empty accounts block reads as
// "there was nothing to do".
func TestModifyFailsWhenNoMailboxSucceeded(t *testing.T) {
	broken := &stubLabels{err: &mail.ProviderError{Provider: mail.ProviderGmail, Account: "work", Op: "modify", Err: errUpstream}}
	tools := fanoutTools(byAccount{workMailbox.ID: broken, archiveMailbox.ID: broken})

	res, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"}, AddLabels: []string{"Label_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a call that changed nothing must not report success")
	}
	if _, ok := payload(t, res)["accounts"]; !ok {
		t.Error("the per-account detail must travel with the error, or there is nothing to debug")
	}
}

// mail_trash reached one mailbox that cannot destroy at all and one that can. The refusal is
// permanent and the contract has a code for saying so; a bare message left a client retrying
// it forever.
func TestTrashCarriesACodePerMailbox(t *testing.T) {
	work := &stubDestroyer{}
	tools := fanoutTools(byAccount{
		workMailbox.ID:    work,
		archiveMailbox.ID: &stubLabels{}, // no Destroyer: this mailbox cannot trash
	})

	res, _, err := tools.handleTrash(grantOver(mail.CapDestructive), nil, trashArgs{
		IDs: []string{"acct_1:m1", "acct_2:m2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("one mailbox that cannot trash must not lose the one that can: %v", payload(t, res))
	}

	accounts := accountsBlock(t, payload(t, res))
	if got := entry(t, accounts, "work")["trash"]; got != float64(1) {
		t.Errorf("work should report one message trashed, got %v", got)
	}
	if got := entry(t, accounts, "archive")["error"]; got != "unsupported_by_provider" {
		t.Errorf("a provider that cannot trash must say so with the code, got %v", got)
	}
}

// Listing labels fans out, and used to return from inside the fan-out on the first failure,
// discarding the mailboxes it had already listed.
func TestLabelsListingSurvivesAFailingMailbox(t *testing.T) {
	work := &stubLabels{labels: []mail.Label{{ID: "Label_1", Name: "receipts"}}}
	archive := &stubLabels{err: mail.ErrNeedsReauth}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: archive})

	res, _, err := tools.handleLabels(grantOver(mail.CapRead), nil, labelsArgs{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("one unreachable mailbox must not lose the other: %v", payload(t, res))
	}

	accounts := accountsBlock(t, payload(t, res))
	listed, ok := entry(t, accounts, "work")["labels"].([]any)
	if !ok || len(listed) != 1 {
		t.Errorf("the healthy mailbox should have returned its labels, got %v", accounts["work"])
	}
	if got := entry(t, accounts, "archive")["error"]; got != "auth_expired" {
		t.Errorf("expired credentials must be reported as such, got %v", got)
	}
}

// A grant holding `labels` without `read` was offered no label tool at all, because
// registration sat under `read` and the tool set is filtered at construction. Creating a
// label needs only `labels`, and now reaches the handler that enforces exactly that.
func TestALabelsOnlyGrantCanCreateALabel(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels), nil, labelsArgs{
		Action: "create", Account: "work", Name: "receipts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("creating a label needs only the labels capability: %v", payload(t, res))
	}
	if got := entry(t, accountsBlock(t, payload(t, res)), "work")["created"]; got != "Label_receipts" {
		t.Errorf("the created label should be reported, got %v", got)
	}
}

// The same grant may not list, because listing is reading. The capability split is the point:
// being offered the tool is not being granted every action on it.
func TestALabelsOnlyGrantCannotList(t *testing.T) {
	tools := fanoutTools(byAccount{workMailbox.ID: &stubLabels{}, archiveMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels), nil, labelsArgs{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("listing labels without the read capability must be refused")
	}
	if got := payload(t, res)["error"]; got != "scope_denied" {
		t.Errorf("want scope_denied, got %v", got)
	}
}
