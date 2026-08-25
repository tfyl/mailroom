package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// startIMAP runs an in-memory IMAP server for the duration of a test, so the linking form is
// exercised against something that accepts one password and refuses the rest.
func startIMAP(t *testing.T) (addr, username, password string) {
	t.Helper()

	mem := imapmemserver.New()
	u := imapmemserver.NewUser("operator", "hunter2")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); _ = listener.Close() })

	return listener.Addr().String(), "operator", "hunter2"
}

func postLinkIMAP(t *testing.T, s *Server, who user.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/accounts/link/imap", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.linkIMAP(rec, r)
	return rec
}

func TestLinkingAnIMAPMailboxFromTheForm(t *testing.T) {
	addr, username, password := startIMAP(t)
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	form := url.Values{
		"alias":    {"personal"},
		"address":  {"operator@example.com"},
		"host":     {addr},
		"username": {username},
		"password": {"hun ter2"},
		"insecure": {"1"},
	}
	if rec := postLinkIMAP(t, s, me, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after linking, got %d: %s", rec.Code, rec.Body)
	}

	acct, err := db.AccountByAlias(ctx, me.ID, "personal")
	if err != nil {
		t.Fatalf("the mailbox was not stored: %v", err)
	}

	// What the mailbox is worth is whether the credential can be opened again and still
	// describes the server that was verified.
	sealed, err := db.Credential(ctx, me.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := s.sealer.OpenString(sealed, string(acct.ID))
	if err != nil {
		t.Fatalf("the credential does not open with the account id as context: %v", err)
	}
	var cfg imapprovider.Config
	if err := json.Unmarshal([]byte(opened), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != addr {
		t.Errorf("host: want %q, got %q", addr, cfg.Host)
	}
	if cfg.Password != password {
		t.Errorf("the password should have had its spaces stripped, got %q", cfg.Password)
	}
	if cfg.SMTPFrom != "operator@example.com" {
		t.Errorf("the envelope sender should default to the address, got %q", cfg.SMTPFrom)
	}
	if cfg.TLS {
		t.Error("the form asked for no TLS and the stored credential says otherwise")
	}
}

// A rejected password must leave nothing behind. The whole reason the form connects before
// storing is that a mailbox which looks linked and fails on first use is worse than an error.
func TestRefusedIMAPCredentialsStoreNothing(t *testing.T) {
	addr, username, _ := startIMAP(t)
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, _ := db.ListUsers(ctx)

	rec := postLinkIMAP(t, s, users[0], url.Values{
		"alias":    {"personal"},
		"address":  {"operator@example.com"},
		"host":     {addr},
		"username": {username},
		"password": {"wrong"},
		"insecure": {"1"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a rejected login, got %d", rec.Code)
	}

	accounts, err := db.ListAccounts(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a refused credential must leave no mailbox behind, got %+v", accounts)
	}

	// "Re-link required" is the provider's phrasing for a mailbox that was working and
	// stopped. This is a first attempt, and saying it here sends somebody looking for a
	// mailbox that never existed.
	body := rec.Body.String()
	if !strings.Contains(body, "rejected the credentials") {
		t.Errorf("the page should say the server rejected the credentials, got: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "re-link") {
		t.Errorf("a first attempt must not be described as needing a re-link: %s", body)
	}
	// The form comes back filled in, minus the password.
	if !strings.Contains(body, `value="personal"`) {
		t.Errorf("the refused form should come back with the values already typed: %s", body)
	}
	if strings.Contains(body, "wrong") {
		t.Errorf("the password must not be echoed back into the page: %s", body)
	}
}

// An empty SMTP host is a real choice rather than a mistake: the provider withholds send, and
// the page has to say so instead of quietly linking a mailbox that cannot do it.
func TestLinkingWithoutSMTPSaysSendingIsOff(t *testing.T) {
	addr, username, password := startIMAP(t)
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, _ := db.ListUsers(ctx)

	rec := postLinkIMAP(t, s, users[0], url.Values{
		"alias":    {"readonly"},
		"address":  {"operator@example.com"},
		"host":     {addr},
		"username": {username},
		"password": {password},
		"insecure": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after linking, got %d: %s", rec.Code, rec.Body)
	}
	next := rec.Header().Get("Location")
	if !strings.Contains(next, "sending=off") {
		t.Fatalf("linking with no SMTP host should say so on the way back, got %q", next)
	}

	page := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, next, nil)
	r = r.WithContext(user.NewContext(r.Context(), users[0]))
	s.accounts(page, r)

	body := page.Body.String()
	if !strings.Contains(body, "cannot send") {
		t.Errorf("the mailboxes page should say the mailbox cannot send: %s", body)
	}
	if !strings.Contains(body, "readonly") {
		t.Errorf("the confirmation should name the mailbox that was linked: %s", body)
	}
}
