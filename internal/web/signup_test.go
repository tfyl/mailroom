package web

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/app"
	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

func testServer(t *testing.T, policy signup.Policy) (*Server, *store.Store) {
	t.Helper()
	return testServerWith(t, policy, &config.Config{})
}

// testServerWith takes the configuration too, for the tests that need a provider client to
// exist before the handler under test will look at anything else.
func testServerWith(t *testing.T, policy signup.Policy, cfg *config.Config) (*Server, *store.Store) {
	t.Helper()
	return testServerAt(t, policy, cfg, "https://mail.example.com")
}

// testServerAt takes the public URL as well, for the tests that care what this instance
// calls itself rather than only what it renders.
func testServerAt(t *testing.T, policy signup.Policy, cfg *config.Config, publicURL string) (*Server, *store.Store) {
	t.Helper()

	db, err := store.Open("sqlite://" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A sealer and a provider set with nothing configured: enough for the pages and the IMAP
	// linking form, which needs neither an OAuth client nor a network.
	sealer, err := secrets.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, secrets.KeyLen)))
	if err != nil {
		t.Fatal(err)
	}
	// The real queue over the real store, with a provider set that has nothing configured.
	// It is enough for every page and every scoping check; the tests that approve a held
	// action and watch it reach a mailbox replace it with one over a recording provider, in
	// held_test.go.
	providers := app.NewProviders(db, sealer, cfg)
	s, err := New(db, providers, sealer,
		auth.NewRegistry(auth.NewSessions(time.Hour)), held.New(db, providers, db, db, time.Hour), policy,
		publicURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

// signInAs drives the middleware the way a request from an authenticated browser would.
func signInAs(s *Server, subject, invite string) *httptest.ResponseRecorder {
	reached := false
	h := s.withUser(func(http.ResponseWriter, *http.Request) { reached = true })

	r := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	if invite != "" {
		r.AddCookie(&http.Cookie{Name: inviteCookie, Value: invite})
	}
	r = r.WithContext(auth.WithOperator(r.Context(), auth.Operator{
		Issuer: "https://idp.example.com", Subject: subject, Email: subject + "@example.com",
	}))

	rec := httptest.NewRecorder()
	h(rec, r)
	if reached {
		rec.Code = http.StatusOK
	}
	return rec
}

// The whole point of the feature: on a closed instance the first person through the door is
// the operator, and the next authenticated stranger gets nothing.
func TestClosedInstanceRefusesAStranger(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Closed})

	if rec := signInAs(s, "ada", ""); rec.Code != http.StatusOK {
		t.Fatalf("the first sign-in should have been let through, got %d", rec.Code)
	}

	rec := signInAs(s, "stranger", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a refused signup, got %d", rec.Code)
	}

	n, err := db.CountUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a refused signup must leave no user behind, count is %d", n)
	}
}

// The refusal page must not reveal whether the person asked about has an account here. It
// says the instance is closed, which is true for everybody, rather than anything about them.
func TestRefusalDoesNotLeakMembership(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Closed})
	signInAs(s, "ada", "")

	body := signInAs(s, "stranger", "").Body.String()
	for _, leak := range []string{"no such", "unknown", "not found", "stranger@example.com"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("the refusal page leaks %q: %s", leak, body)
		}
	}
	if !strings.Contains(strings.ToLower(body), "not accepting new accounts") {
		t.Fatalf("the refusal should say the instance is closed, got: %s", body)
	}
}

// A refused visitor is signed out as well. Leaving the session in place would send every
// subsequent request straight back through a sign-in that cannot succeed.
func TestRefusalEndsTheSession(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Closed})
	signInAs(s, "ada", "")

	rec := signInAs(s, "stranger", "")
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected the session cookie to be cleared, got %v", rec.Result().Cookies())
	}
}

// An invite carried in the cookie is what turns a refusal into an account, and it is spent
// by doing so.
func TestInviteCookieAdmitsOnce(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Invite})
	ctx := context.Background()

	signInAs(s, "ada", "")
	owner, err := db.ListUsers(ctx)
	if err != nil || len(owner) != 1 {
		t.Fatalf("expected one user, got %v %v", owner, err)
	}

	_, code, err := db.CreateInvite(ctx, owner[0].ID, "for bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if rec := signInAs(s, "bob", code); rec.Code != http.StatusOK {
		t.Fatalf("an invited person should be let in, got %d: %s", rec.Code, rec.Body)
	}
	if rec := signInAs(s, "carol", code); rec.Code != http.StatusForbidden {
		t.Fatalf("a spent invite must not admit a second person, got %d", rec.Code)
	}
}

// The code must not survive the request that used it. A cookie left in place would be spent
// on whichever identity signed in next from the same browser.
func TestInviteCookieIsClearedAfterUse(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Invite})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, _ := db.ListUsers(ctx)
	_, code, err := db.CreateInvite(ctx, users[0].ID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	rec := signInAs(s, "bob", code)
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == inviteCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the invite cookie should be cleared after redemption, got %v", rec.Result().Cookies())
	}
}

// The invite link puts the code in a cookie rather than leaving it in the URL, and says
// nothing about whether it is valid — answering that here would let anyone test codes
// without signing in.
func TestInviteLinkStoresTheCodeWithoutValidatingIt(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Invite})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/invite/nosuchcode", nil)
	r.SetPathValue("code", "nosuchcode")
	s.acceptInvite(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect to sign in, got %d", rec.Code)
	}
	var stored string
	for _, c := range rec.Result().Cookies() {
		if c.Name == inviteCookie {
			stored = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("the invite cookie must be HttpOnly and SameSite=Lax, got %#v", c)
			}
		}
	}
	// Normalised on the way in, so a code retyped in another case still matches its hash.
	if stored != "NOSUCHCODE" {
		t.Fatalf("want the normalised code in the cookie, got %q", stored)
	}
}

// Only the account that set up the instance may issue invites. Anyone else asking is
// refused, not shown an empty page.
func TestInvitesPageIsOwnerOnly(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	signInAs(s, "bob", "")
	users, err := db.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("expected two users, got %v %v", users, err)
	}

	for _, tc := range []struct {
		who  user.User
		want int
	}{
		{users[0], http.StatusOK},
		{users[1], http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/invites", nil)
		r = r.WithContext(user.NewContext(r.Context(), tc.who))
		s.invites(rec, r)
		if rec.Code != tc.want {
			t.Errorf("user %s: want %d, got %d", tc.who.Subject, tc.want, rec.Code)
		}
	}
}

// Every form in the UI carries the field name that csrf.check actually reads. Getting this
// wrong produces a form that renders perfectly and is refused on submission, which no
// handler test would catch because handler tests do not go through the check.
func TestFormsCarryTheCSRFFieldNameThatIsChecked(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Invite})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateInvite(ctx, users[0].ID, "someone", time.Hour); err != nil {
		t.Fatal(err)
	}

	for _, page := range []struct {
		path   string
		render http.HandlerFunc
	}{
		{"/invites", s.invites},
		{"/accounts", s.accounts},
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, page.path, nil)
		r = r.WithContext(user.NewContext(r.Context(), users[0]))
		page.render(rec, r)

		body := rec.Body.String()
		if strings.Count(body, "<form") == 0 {
			t.Errorf("%s: rendered no forms at all, so this proves nothing", page.path)
		}
		if forms := strings.Count(body, "<form"); forms != strings.Count(body, `name="csrf_token"`) {
			t.Errorf("%s: every form needs a csrf_token field: %d forms, %d fields",
				page.path, forms, strings.Count(body, `name="csrf_token"`))
		}
		if strings.Contains(body, `name="csrf"`) {
			t.Errorf(`%s: the field is called csrf_token, not csrf — csrf.check reads csrf_token`, page.path)
		}
	}
}
