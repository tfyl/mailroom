package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestCSRF(t *testing.T) *csrf {
	t.Helper()
	c, err := newCSRF()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The bug this replaces: the token bound solely to the session cookie, which only the OIDC
// path ever sets. A deployment behind an authenticating proxy establishes no session here —
// the proxy already authenticated — so every form rendered an empty token and every mutating
// request was refused. The whole interface was read-only for anyone whose proxy was doing
// exactly what it should.
func TestTokenWorksWithNoSessionCookie(t *testing.T) {
	c := newTestCSRF(t)

	rec := httptest.NewRecorder()
	token := c.token(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil), false)
	if token == "" {
		t.Fatal("a browser with no session must still get a usable token")
	}

	var issued *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == csrfCookie {
			issued = cookie
		}
	}
	if issued == nil {
		t.Fatal("the seed cookie must be issued alongside the token")
	}
	if !issued.HttpOnly || issued.SameSite != http.SameSiteLaxMode {
		t.Errorf("the seed cookie must be HttpOnly and SameSite=Lax, got %#v", issued)
	}

	// The submission that follows carries that cookie, and must be accepted.
	post := httptest.NewRequest(http.MethodPost, "/accounts/unlink",
		strings.NewReader("csrf_token="+token))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(issued)
	if !c.check(post) {
		t.Fatal("the token just issued must satisfy the check that follows it")
	}
}

// A session, where there is one, is the better seed: signing out should invalidate tokens
// minted while signed in.
func TestSessionCookieIsPreferredAsTheSeed(t *testing.T) {
	c := newTestCSRF(t)

	withSession := httptest.NewRequest(http.MethodGet, "/", nil)
	withSession.AddCookie(&http.Cookie{Name: "mailroom_session", Value: "session-one"})
	withSession.AddCookie(&http.Cookie{Name: csrfCookie, Value: "unrelated"})

	rec := httptest.NewRecorder()
	if got, want := c.token(rec, withSession, false), c.sign("session-one"); got != want {
		t.Fatal("the session must win over the fallback seed")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("nothing needs issuing when the browser already has a seed")
	}

	// A different session is a different token, which is what makes signing out effective.
	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.AddCookie(&http.Cookie{Name: "mailroom_session", Value: "session-two"})
	if c.token(httptest.NewRecorder(), other, false) == c.sign("session-one") {
		t.Fatal("two sessions must not share a token")
	}
}

// A cross-site POST carries no cookie under SameSite=Lax, so it arrives with nothing to bind
// to. Minting a seed at that point would let the request prove itself.
func TestCheckRefusesARequestCarryingNoCookie(t *testing.T) {
	c := newTestCSRF(t)

	token := c.token(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), false)
	post := httptest.NewRequest(http.MethodPost, "/accounts/unlink",
		strings.NewReader("csrf_token="+token))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if c.check(post) {
		t.Fatal("a request with no seed cookie must be refused whatever token it presents")
	}
}

// Holding a valid seed is not the same as holding the token derived from it: the seed is in
// an HttpOnly cookie the attacker's page cannot read, and the signing key never leaves this
// process.
func TestCheckRefusesAWrongToken(t *testing.T) {
	c := newTestCSRF(t)
	seed := &http.Cookie{Name: csrfCookie, Value: "a-real-seed"}

	for _, presented := range []string{"", "not-the-token", c.sign("a-different-seed")} {
		post := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("csrf_token="+presented))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		post.AddCookie(seed)
		if c.check(post) {
			t.Fatalf("token %q must be refused", presented)
		}
	}
}

// The cookie follows the deployment's scheme, not the request's: behind a terminating proxy
// the request arrives over plain HTTP while the browser is on HTTPS.
func TestSeedCookieIsSecureWhenTheInstanceIs(t *testing.T) {
	c := newTestCSRF(t)
	rec := httptest.NewRecorder()
	c.token(rec, httptest.NewRequest(http.MethodGet, "/", nil), true)

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == csrfCookie && !cookie.Secure {
			t.Fatal("the seed cookie must be Secure on an https instance")
		}
	}
}
