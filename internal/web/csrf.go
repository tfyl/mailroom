package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRF protection uses a synchronizer token derived from a cookie only this browser holds.
//
// Deriving rather than storing keeps it stateless while still binding the token to one
// browser: an attacker who can make a victim's browser submit a form cannot read the
// victim's cookies, so cannot compute the token. The process key means tokens do not survive
// a restart, which costs a re-submit and nothing else.
//
// The seed is the session cookie where there is one. Where there is not — a deployment behind
// an authenticating proxy never establishes a session here, because the proxy already did —
// this issues a cookie of its own. Binding solely to the session was a real bug: every
// mutating form on a forward-auth instance rendered an empty token and was refused, so the
// whole interface was read-only for anyone whose proxy was doing exactly what it should.
type csrf struct {
	key []byte
}

// csrfCookie holds the seed when no session cookie exists. Its value is not a credential and
// authenticates nobody: it exists only to be something the attacker's page cannot read.
const csrfCookie = "mailroom_csrf"

func newCSRF() (*csrf, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &csrf{key: key}, nil
}

// token returns the token to embed in a form, issuing the cookie it binds to if the browser
// has nothing suitable yet.
func (c *csrf) token(w http.ResponseWriter, r *http.Request, secure bool) string {
	seed, ok := c.seed(r)
	if !ok {
		fresh, err := randomSeed()
		if err != nil {
			// Without entropy the token would be guessable, so render no token at all: the
			// form is then refused, which is the safe direction to fail in.
			return ""
		}
		// Lax rather than Strict: the cookie has to survive the return leg of an OIDC
		// sign-in, which is a top-level navigation from the issuer. It still means a
		// cross-site POST carries no cookie, so an attacker's form cannot reach a seed at
		// all — the token check is then the second of two failures rather than the only one.
		http.SetCookie(w, &http.Cookie{
			Name: csrfCookie, Value: fresh, Path: "/",
			HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		})
		seed = fresh
	}
	return c.sign(seed)
}

// seed picks what the token binds to, preferring the session so that signing out invalidates
// tokens minted while signed in.
func (c *csrf) seed(r *http.Request) (string, bool) {
	for _, name := range []string{"mailroom_session", csrfCookie} {
		if cookie, err := r.Cookie(name); err == nil && cookie.Value != "" {
			return cookie.Value, true
		}
	}
	return "", false
}

func (c *csrf) sign(seed string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(seed))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func randomSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// check verifies the submitted token. Any mismatch is refused rather than logged and
// allowed: a form post that fails this test is either an attack or a stale tab, and neither
// should change mailbox permissions.
//
// Deliberately does not issue a seed. A request arriving with no cookie has nothing to prove
// it came from a page this server rendered, and minting one here would let it prove itself.
func (c *csrf) check(r *http.Request) bool {
	seed, ok := c.seed(r)
	if !ok {
		return false
	}
	want := c.sign(seed)
	got := r.FormValue("csrf_token")
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (c *csrf) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !c.check(r) {
			http.Error(w, "this form expired or came from another page. Reload and try again.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
