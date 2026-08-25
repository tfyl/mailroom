package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// gmailMailbox is a linked Google mailbox in a real database, with its refresh token sealed
// the way linking seals it.
func gmailMailbox(t *testing.T, refresh string) (*Providers, *store.Store, *secrets.Sealer, mail.Account) {
	t.Helper()

	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite://" + filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	owner, _, err := db.EnsureUser(ctx, user.User{Issuer: "test", Subject: "operator"},
		store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		t.Fatalf("creating the owner: %v", err)
	}

	acct := mail.Account{
		ID: "acct_gmail", OwnerID: owner.ID, Alias: "work", Address: "operator@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	sealed, err := sealer.SealString(refresh, string(acct.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LinkAccount(ctx, owner.ID, acct, sealed, ""); err != nil {
		t.Fatalf("linking the mailbox: %v", err)
	}

	return NewProviders(db, sealer, &config.Config{}), db, sealer, acct
}

// tokenEndpoint stands in for the authorisation server. issue is the refresh token it hands
// back with every access token; empty means it returns none, which is what Google does.
func tokenEndpoint(t *testing.T, issue string) (*oauth2.Config, *atomic.Int64) {
	t.Helper()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := map[string]any{"access_token": "an-access-token", "token_type": "Bearer", "expires_in": 3600}
		if issue != "" {
			body["refresh_token"] = issue
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	return &oauth2.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Endpoint:     oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams},
	}, &calls
}

func storedRefresh(t *testing.T, db *store.Store, sealer *secrets.Sealer, acct mail.Account) string {
	t.Helper()
	sealed, err := db.Credential(context.Background(), acct.OwnerID, acct.ID)
	if err != nil {
		t.Fatalf("reading the credential: %v", err)
	}
	refresh, err := sealer.OpenString(sealed, string(acct.ID))
	if err != nil {
		t.Fatalf("the stored credential does not open under the account id: %v", err)
	}
	return refresh
}

// A rotated refresh token used to be read into memory and thrown away with the provider that
// held it. Against an authorisation server that rotates — increasingly the default, though
// not Google — the mailbox goes on working until the token in the database is presented a
// second time, and then fails as a credential error with nothing to point at.
func TestARotatedRefreshTokenIsStored(t *testing.T) {
	providers, db, sealer, acct := gmailMailbox(t, "issued-at-linking")
	conf, _ := tokenEndpoint(t, "rotated")

	source := providers.refreshing(context.Background(), conf, acct, "issued-at-linking")
	if _, err := source.Token(); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	if got := storedRefresh(t, db, sealer, acct); got != "rotated" {
		t.Fatalf("the store still holds %q; the rotated token was dropped", got)
	}
	// Sealed under the account id, as the column was written: anything else unseals nowhere.
	sealed, err := db.Credential(context.Background(), acct.OwnerID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.OpenString(sealed, "acct_somebody_else"); err == nil {
		t.Error("the rewritten credential opened under the wrong account id")
	}
}

// The common case is a provider that returns no new refresh token at all, and a write on
// every refresh would be a database round trip per token for nothing.
func TestAnUnchangedRefreshTokenIsNotRewritten(t *testing.T) {
	providers, db, _, acct := gmailMailbox(t, "issued-at-linking")
	conf, _ := tokenEndpoint(t, "")

	before, err := db.Credential(context.Background(), acct.OwnerID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}

	source := providers.refreshing(context.Background(), conf, acct, "issued-at-linking")
	if _, err := source.Token(); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	after, err := db.Credential(context.Background(), acct.OwnerID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Each seal draws a fresh nonce, so an identical row is proof no write happened rather
	// than proof the same value was written.
	if after != before {
		t.Error("the credential was rewritten although the refresh token had not changed")
	}
}

// A provider is cached and shared, so several tool calls reach the same token source at once.
func TestConcurrentCallsRotateOnce(t *testing.T) {
	providers, db, sealer, acct := gmailMailbox(t, "issued-at-linking")
	conf, calls := tokenEndpoint(t, "rotated")

	source := providers.refreshing(context.Background(), conf, acct, "issued-at-linking")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := source.Token(); err != nil {
				t.Errorf("refreshing: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := storedRefresh(t, db, sealer, acct); got != "rotated" {
		t.Fatalf("the store holds %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the authorisation server was asked for a token %d times; one is enough for eight callers", n)
	}
}
