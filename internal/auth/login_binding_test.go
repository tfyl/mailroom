package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A sign-in has to be finished by the browser that started it.
//
// PKCE binds the authorization code to the attempt and the nonce binds the id_token to it,
// but neither binds the attempt to a browser. Without that, the callback URL is one anyone
// may complete: an attacker signs in at the issuer as themselves, keeps the callback instead
// of following it, and gets the victim to open it — a top-level GET carrying no cookie, so
// SameSite does not apply. The victim's browser is then signed in as the attacker, and the
// next mailbox they link is stored in the attacker's account.
//
// The check has to happen before the code is exchanged, which is what these assert: a
// refusal that names the browser, rather than any failure further down the flow.
func TestACallbackFromAnotherBrowserIsRefused(t *testing.T) {
	const state = "state-value"

	for _, tc := range []struct {
		name   string
		cookie string
		set    bool
	}{
		{name: "no binder cookie at all", set: false},
		{name: "a binder from a different attempt", cookie: "someone-elses-binder", set: true},
		{name: "an empty binder", cookie: "", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &OIDC{pending: newPendingLogins(10 * time.Minute)}
			o.pending.put(state, pendingLogin{
				Next: "/", Verifier: "v", Nonce: "n", Binder: "the-real-binder",
			})

			r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+state+"&code=xyzzy", nil)
			if tc.set {
				r.AddCookie(&http.Cookie{Name: loginCookie, Value: tc.cookie})
			}
			w := httptest.NewRecorder()

			_, err := o.Callback(w, r)
			if err == nil {
				t.Fatal("the callback was accepted from a browser that did not start it")
			}
			if !strings.Contains(err.Error(), "different browser") {
				t.Fatalf("want a refusal naming the browser, got: %v", err)
			}
			if strings.Contains(w.Result().Header.Get("Set-Cookie"), sessionCookie+"=") {
				t.Error("a session cookie was issued despite the refusal")
			}
		})
	}
}

// The matching binder has to get past this check, or the fix would simply break sign-in.
// It fails afterwards, at the code exchange against an issuer that is not there, which is
// exactly how far this test can honestly reach.
func TestTheBrowserThatStartedTheLoginGetsPastTheCheck(t *testing.T) {
	const state = "state-value"
	o := &OIDC{pending: newPendingLogins(10 * time.Minute), oauth: testOAuthConfig()}
	o.pending.put(state, pendingLogin{Next: "/", Verifier: "v", Nonce: "n", Binder: "the-real-binder"})

	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+state+"&code=xyzzy", nil)
	r.AddCookie(&http.Cookie{Name: loginCookie, Value: "the-real-binder"})

	_, err := o.Callback(httptest.NewRecorder(), r)
	if err == nil {
		t.Fatal("expected the flow to continue and then fail at the exchange")
	}
	if strings.Contains(err.Error(), "different browser") {
		t.Fatalf("the browser that started the login was refused: %v", err)
	}
}

// The binder is set on the way out, or there is nothing for the callback to compare against.
func TestStartLoginIssuesABinderCookie(t *testing.T) {
	o := &OIDC{pending: newPendingLogins(10 * time.Minute), oauth: testOAuthConfig()}

	w := httptest.NewRecorder()
	o.StartLogin(w, httptest.NewRequest(http.MethodGet, "/accounts", nil))

	var binder *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == loginCookie {
			binder = c
		}
	}
	if binder == nil {
		t.Fatal("no binder cookie was set, so the callback has nothing to check")
	}
	if binder.Value == "" {
		t.Error("the binder is empty")
	}
	if !binder.HttpOnly {
		t.Error("the binder must not be readable from script")
	}
	if binder.SameSite != http.SameSiteLaxMode {
		t.Error("the binder must survive the top-level redirect back from the issuer")
	}
}

func testOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:    "client",
		RedirectURL: "https://mail.example.com/auth/oidc/callback",
		// A token endpoint that refuses the connection, so the exchange fails quickly and
		// for a reason that is plainly not the binder check.
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://issuer.example.com/authorize",
			TokenURL: "http://127.0.0.1:1/token",
		},
	}
}
