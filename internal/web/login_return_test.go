package web

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
)

// What an MCP client sends a signed-out operator to. Everything that identifies the request
// is in the query, so a sign-in that returns to the path alone lands on an /authorize that
// names no client.
const authorizeRequest = "/authorize?client_id=hermes-client&redirect_uri=" +
	"http%3A%2F%2F127.0.0.1%3A8765%2Fcallback&response_type=code&state=xyz" +
	"&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256"

// After signing in, the operator is back at the authorization request they started from,
// query and all — with one identity provider, where the guard goes straight to it.
func TestSigningInReturnsToTheFullAuthorizeRequest(t *testing.T) {
	h, issuer := signInSurfaceWithIssuer(t, "test")

	rec := get(h, authorizeRequest)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a signed-out /authorize got %d, want a 303 to the issuer", rec.Code)
	}

	if got := finishLogin(t, h, issuer, rec); got != authorizeRequest {
		t.Fatalf("signing in returned to %q\nwant %q", got, authorizeRequest)
	}
}

// With more than one identity provider the guard sends the operator to the login page to
// choose, and the destination has to survive that hop too: into /login?next=, out through
// the link the page renders, and into the provider's start.
func TestTheLoginPageCarriesTheFullAuthorizeRequest(t *testing.T) {
	h, issuer := signInSurfaceWithIssuer(t, "test", "other")

	rec := get(h, authorizeRequest)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a signed-out /authorize got %d, want a 303 to the login page", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/login" {
		t.Fatalf("redirected to %q, want the login page", loc)
	}
	if got := loc.Query().Get("next"); got != authorizeRequest {
		t.Fatalf("the login page was given next=%q\nwant %q", got, authorizeRequest)
	}

	page := get(h, loc.String())
	if page.Code != http.StatusOK {
		t.Fatalf("the login page got %d", page.Code)
	}
	m := regexp.MustCompile(`href="(/auth/test/start[^"]*)"`).FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatalf("no link to the test provider on the login page:\n%s", page.Body.String())
	}
	start := get(h, unescapeHTML(m[1]))
	if start.Code != http.StatusSeeOther {
		t.Fatalf("starting the login got %d, want a 303 to the issuer", start.Code)
	}

	if got := finishLogin(t, h, issuer, start); got != authorizeRequest {
		t.Fatalf("signing in returned to %q\nwant %q", got, authorizeRequest)
	}
}

// finishLogin follows a redirect to the issuer through to the callback and returns where the
// callback sends the browser.
func finishLogin(t *testing.T, h http.Handler, issuer *testIssuer, toIssuer *httptest.ResponseRecorder) string {
	t.Helper()

	to, err := url.Parse(toIssuer.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(to.String(), issuer.url) {
		t.Fatalf("expected a redirect to the issuer, got %q", to)
	}
	issuer.nonce = to.Query().Get("nonce")

	var binder *http.Cookie
	for _, c := range toIssuer.Result().Cookies() {
		if c.Name == "mailroom_login" {
			binder = c
		}
	}
	if binder == nil {
		t.Fatal("no binder cookie was issued")
	}

	rec := callback(t, h, url.Values{"state": {to.Query().Get("state")}, "code": {"the-code"}}, binder)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the callback got %d, want a 303 back to the app: %s", rec.Code, rec.Body.String())
	}
	return rec.Header().Get("Location")
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func unescapeHTML(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#43;", "+", "&#39;", "'", "&#34;", `"`).Replace(s)
}

// signInSurfaceWithIssuer mounts the browser routes with one provider per id, all against an
// issuer that completes the flow: it signs an id_token carrying whatever nonce the test last
// saw on the way out.
func signInSurfaceWithIssuer(t *testing.T, ids ...string) (http.Handler, *testIssuer) {
	t.Helper()

	issuer := newTestIssuer(t, "mailroom-test")
	s, db := testServer(t, signup.Policy{Mode: signup.Open})

	sessions := auth.NewSessions(time.Hour)
	registry := auth.NewRegistry(sessions)
	for _, id := range ids {
		provider, err := auth.NewOIDC(context.Background(), auth.OIDCOptions{
			ID:          id,
			Label:       id,
			Issuer:      issuer.url,
			ClientID:    "mailroom-test",
			RedirectURL: "https://mail.example.com/auth/" + id + "/callback",
			Sessions:    sessions,
		})
		if err != nil {
			t.Fatal(err)
		}
		registry.AddOIDC(provider)
	}
	s.operator = registry

	mux := http.NewServeMux()
	s.Routes(mux, oauthsrv.New(db, "https://mail.example.com"))
	return mux, issuer
}

type testIssuer struct {
	url   string
	nonce string
}

// newTestIssuer serves discovery, a key set and a token endpoint: enough of an issuer for a
// sign-in to complete, signed with a key made for the test.
func newTestIssuer(t *testing.T, clientID string) *testIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := &testIssuer{url: srv.URL}

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
			"n": b64(key.N.Bytes()),
			"e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
		claims, _ := json.Marshal(map[string]any{
			"iss":   srv.URL,
			"sub":   "operator",
			"aud":   clientID,
			"iat":   time.Now().Unix(),
			"exp":   time.Now().Add(time.Hour).Unix(),
			"nonce": issuer.nonce,
			"email": "operator@example.com",
		})
		signing := b64(header) + "." + b64(claims)
		digest := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     signing + "." + b64(sig),
		})
	})
	return issuer
}
