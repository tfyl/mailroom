package app_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/tfyl/mailroom/internal/app"
	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/store"
)

// TestLiveConformance runs the behavioural half of the provider contract against a real
// linked mailbox.
//
// Skipped unless MAILROOM_LIVE_ACCOUNT names an alias, because it needs credentials and
// touches a real account. Everything else it needs — database, encryption key, provider
// OAuth client — comes from the same environment the server reads, so pointing it at a
// running instance is:
//
//	set -a; . ./.env; set +a
//	MAILROOM_LIVE_ACCOUNT=work go test ./internal/app/ -run TestLiveConformance -v
//
// It loads the whole configuration, so a login method has to be configured even though this
// tests providers rather than login. The coupling is incidental and worth knowing about
// before it looks like a provider failure: an environment predating the removal of password
// login fails here with a message about MAILROOM_PASSWORD_HASH.
//
// The suite is read-mostly. It searches, fetches, walks a thread and lists labels; the only
// mutation it attempts is creating an exclusive label, which providers that lack them are
// required to refuse.
func TestLiveConformance(t *testing.T) {
	alias := os.Getenv("MAILROOM_LIVE_ACCOUNT")
	if alias == "" {
		t.Skip("set MAILROOM_LIVE_ACCOUNT to the alias of a linked mailbox to run this")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	sealer, err := secrets.NewSealer(cfg.EncryptionKey)
	if err != nil {
		t.Fatalf("encryption key: %v", err)
	}
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// Mailboxes belong to a user, so the harness needs one to look through. Any of them will
	// do: it resolves the alias against each until it finds the owner, which keeps the
	// command line to a single variable rather than making the caller find a user id.
	account, err := findAccount(ctx, db, alias)
	if err != nil {
		t.Fatalf("no linked mailbox with alias %q: %v", alias, err)
	}

	provider, err := app.NewProviders(db, sealer, cfg).For(ctx, account)
	if err != nil {
		t.Fatalf("building a provider for %s: %v", alias, err)
	}

	t.Logf("running the contract against %s (%s, %s)", account.Alias, account.Address, account.Provider)

	conformance.Static(t, provider)
	conformance.Live(t, conformance.Harness{
		Provider:  provider,
		Account:   account,
		SearchAll: mail.Query{Limit: 10},
		// Well-formed for this provider but never issued, so Get must report not-found
		// rather than a parse failure.
		MissingID: mail.ScopedID{Account: account.ID, Native: missingNative(account.Provider)},
	})
}

// findAccount locates a mailbox by alias across every user on the instance.
func findAccount(ctx context.Context, db *store.Store, alias string) (mail.Account, error) {
	users, err := db.ListUsers(ctx)
	if err != nil {
		return mail.Account{}, err
	}
	for _, u := range users {
		if acct, err := db.AccountByAlias(ctx, u.ID, alias); err == nil {
			return acct, nil
		}
	}
	return mail.Account{}, fmt.Errorf("no mailbox aliased %q belongs to any user", alias)
}

// missingNative builds an id that is syntactically valid for the provider and certain not to
// exist. The shape differs per provider — Gmail takes a bare id, IMAP and Zoho need a
// mailbox or folder as well, Microsoft takes a bare one again — which is exactly why the
// harness supplies it rather than the suite guessing.
//
// "Valid" has to be taken seriously, or the test checks the wrong thing. Gmail ids are 16
// hex characters holding a value that fits in a signed 64-bit integer: ffffffffffffffff
// overflows that and comes back as 400 "Invalid id value", which is malformed rather than
// missing. Those are different failures with different fixes, and telling them apart is the
// whole point of the check — so this stays comfortably in range.
func missingNative(p mail.ProviderID) string {
	switch p {
	case mail.ProviderIMAP:
		return "INBOX/999999999"
	case mail.ProviderZoho:
		return "0/999999999999999999"
	case mail.ProviderMicrosoft:
		// A Graph id is base64url over a binary entry id. This one is the right shape and
		// carries an entry that was never issued, so Graph reports ErrorItemNotFound rather
		// than refusing to parse it — which is a different failure with a different fix, and
		// telling the two apart is the whole point of the check.
		return "AAMkADAwMDAwMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAw-mailroom-conformance-missing="
	default:
		return "1000000000000000"
	}
}
