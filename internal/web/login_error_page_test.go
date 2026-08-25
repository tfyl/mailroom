package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
)

// What an attacker puts in the URL. Plain words on purpose: html/template escapes markup, so
// the interesting case is the one escaping does nothing about — a sentence rendered above a
// genuine sign-in form, on the operator's own domain, in the operator's own styling.
const plantedSentence = "Your mailbox is suspended. Call IT support on 555-0100 to restore it."

// planted lists the fragments that must not survive into a response, chosen so that no
// escaping of the original could hide one: none of them contains a character html/template
// would rewrite.
var planted = []string{"555-0100", "IT support", "suspended", "restore it"}

// fakeIssuer serves just enough of an OIDC discovery document for NewOIDC to accept it. No
// keys and no token endpoint behind it: every test here fails the flow long before either.
func fakeIssuer(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

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
	return srv.URL
}

// signInSurface mounts the browser routes with one identity provider configured, which is
// what registers the callback these tests drive.
func signInSurface(t *testing.T) http.Handler {
	t.Helper()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})

	registry := auth.NewRegistry(auth.NewSessions(time.Hour))
	provider, err := auth.NewOIDC(context.Background(), auth.OIDCOptions{
		ID:          "test",
		Label:       "Test issuer",
		Issuer:      fakeIssuer(t),
		ClientID:    "mailroom-test",
		RedirectURL: "https://mail.example.com/auth/test/callback",
		Sessions:    auth.NewSessions(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry.AddOIDC(provider)
	s.operator = registry

	mux := http.NewServeMux()
	s.Routes(mux, oauthsrv.New(db, "https://mail.example.com"))
	return mux
}

// startLogin drives the real outbound leg, so the state and the binder that come back are
// the ones this instance actually minted rather than values a test invented.
func startLogin(t *testing.T, h http.Handler) (state string, binder *http.Cookie) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/test/start", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting a login got %d, want 303: %s", rec.Code, rec.Body.String())
	}

	to, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state = to.Query().Get("state")
	if state == "" {
		t.Fatal("the redirect to the issuer carried no state")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "mailroom_login" {
			binder = c
		}
	}
	if binder == nil {
		t.Fatal("no binder cookie was issued")
	}
	return state, binder
}

func callback(t *testing.T, h http.Handler, query url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/auth/test/callback?"+query.Encode(), nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func assertNothingPlanted(t *testing.T, body string) {
	t.Helper()
	for _, fragment := range planted {
		if strings.Contains(body, fragment) {
			t.Errorf("the attacker's %q is in the response body:\n%s", fragment, body)
		}
	}
}

// The link an attacker actually sends: no session, no prior visit, nothing agreed to, and
// under the old ordering nothing checked either — the error was read before the state was.
func TestABareErrorLinkPutsNothingOfTheAttackersOnTheLoginPage(t *testing.T) {
	h := signInSurface(t)

	rec := callback(t, h, url.Values{
		"error":             {"access_denied"},
		"error_description": {plantedSentence},
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	assertNothingPlanted(t, body)
	// The page still has to be the sign-in page rather than a blank error, or the fix has
	// simply removed the one route back into the product.
	if !strings.Contains(body, "Continue with Test issuer") {
		t.Fatalf("the sign-in page lost its methods:\n%s", body)
	}
	if !strings.Contains(body, "no longer valid") {
		t.Errorf("a link matching no sign-in should say so:\n%s", body)
	}
}

// And with a state this instance really minted, which is what an attacker gets by starting a
// login of their own first. The state check is worth having and is not the thing that closes
// this: dropping the free text is.
func TestAValidStateDoesNotLetTheProvidersTextOntoThePage(t *testing.T) {
	h := signInSurface(t)
	state, binder := startLogin(t, h)

	rec := callback(t, h, url.Values{
		"state":             {state},
		"error":             {"access_denied"},
		"error_description": {plantedSentence},
	}, binder)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	assertNothingPlanted(t, rec.Body.String())
}

// Moving the sentence from error_description into the error code must not work either.
func TestTheErrorCodeIsNotEchoedOntoThePage(t *testing.T) {
	h := signInSurface(t)
	state, binder := startLogin(t, h)

	rec := callback(t, h, url.Values{
		"state": {state},
		"error": {"your_mailbox_is_suspended_call_555_0100"},
	}, binder)

	body := rec.Body.String()
	for _, fragment := range []string{"555_0100", "555-0100", "suspended"} {
		if strings.Contains(body, fragment) {
			t.Errorf("the attacker's %q is in the response body:\n%s", fragment, body)
		}
	}
}

// The filtering has to leave something useful behind. A genuine access_denied is the most
// common real failure there is — somebody pressed Cancel at their issuer — and a page that
// cannot say so sends them to the operator instead.
func TestAGenuineAccessDeniedStillExplainsItself(t *testing.T) {
	h := signInSurface(t)
	state, binder := startLogin(t, h)

	rec := callback(t, h, url.Values{
		"state":             {state},
		"error":             {"access_denied"},
		"error_description": {"The user denied the request"},
	}, binder)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "declined at your identity provider") {
		t.Fatalf("a declined sign-in does not say so:\n%s", body)
	}
	// Even wording as innocuous as this issuer's is still the issuer's, and the rule is that
	// none of it is rendered rather than that the harmful parts are picked out.
	if strings.Contains(body, "The user denied the request") {
		t.Error("the provider's own wording was rendered after all")
	}
}
