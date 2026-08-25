package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

func cspFor(t *testing.T, origins []string) string {
	t.Helper()
	h := SecurityHeaders(origins, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	return rec.Header().Get("Content-Security-Policy")
}

// Linking posts a form to mailroom, which answers with a redirect to the provider's consent
// screen. `form-action` governs the whole redirect chain that follows a form submission, so
// a policy of 'self' alone blocks the handoff — the browser refuses it while the server logs
// a perfectly successful response, which is a genuinely confusing failure to debug.
func TestCSPAllowsProviderConsentScreens(t *testing.T) {
	csp := cspFor(t, []string{"https://accounts.google.com", "https://accounts.zoho.eu"})

	if !strings.Contains(csp, "form-action 'self' https://accounts.google.com https://accounts.zoho.eu") {
		t.Fatalf("provider origins must appear in form-action, got: %s", csp)
	}
}

// The directive stays as narrow as the configuration allows: an instance with no provider
// configured advertises nothing extra.
func TestCSPWithoutProvidersAllowsOnlySelf(t *testing.T) {
	csp := cspFor(t, nil)

	if !strings.Contains(csp, "form-action 'self';") {
		t.Fatalf("want form-action limited to 'self', got: %s", csp)
	}
	if strings.Contains(csp, "accounts.google.com") {
		t.Fatalf("an unconfigured instance must not list provider origins, got: %s", csp)
	}
}

// The rest of the policy must survive the change: still no framing, still closed by default.
func TestCSPStillForbidsFraming(t *testing.T) {
	csp := cspFor(t, []string{"https://accounts.google.com"})

	for _, want := range []string{
		"default-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("policy lost %q: %s", want, csp)
		}
	}
}

// One script, from here, and nothing else.
//
// This directive used to be absent: default-src 'none' denied script outright, because the
// UI shipped none. It is 'self' now that /static/app.<digest>.js exists, and what it does not
// say is the point of it. No 'unsafe-inline', so an injected <script> block or an on*
// attribute still cannot run and still cannot rewrite what a button on the consent screen
// appears to do — that, rather than the absence of the directive, is what was ever protecting
// this page. No 'unsafe-eval'. And no origin but this one, so there is no CDN in the trusted
// set and nothing to fetch from a host this deployment does not control.
func TestCSPAllowsOnlyOurOwnScript(t *testing.T) {
	csp := cspFor(t, []string{"https://accounts.google.com"})

	// 'self' and nothing after it. The provider origins a linking flow needs belong to
	// form-action; none of them may leak into the set of places script may come from.
	if !strings.Contains(csp, "script-src 'self';") {
		t.Fatalf("script-src should be 'self' and nothing else, got: %s", csp)
	}
	for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'", "'strict-dynamic'"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("script-src must not admit %s: %s", forbidden, csp)
		}
	}
}

// The stylesheet is a file this server serves, so nothing needs permission to run style it
// did not fetch from here. 'unsafe-inline' would put that permission back for the sake of a
// convenience nothing uses — and restyling a page is not a cosmetic power on a UI whose most
// important screen is one where what a control does has to be obvious before it is pressed.
func TestCSPAllowsOnlyOurOwnStylesheet(t *testing.T) {
	csp := cspFor(t, nil)

	if !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("the stylesheet is served from this origin: %s", csp)
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("no template carries a style attribute, so nothing needs unsafe-inline: %s", csp)
	}
}

// default-src 'none' covers everything the policy does not name, and script-src and style-src
// are named. All of it has to hold at once: naming either without keeping default-src at
// 'none' would quietly reopen every other fetch this page cannot make.
func TestCSPStillDeniesEverythingItDoesNotName(t *testing.T) {
	csp := cspFor(t, nil)

	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("default-src must stay closed: %s", csp)
	}
	for _, absent := range []string{"font-src", "connect-src", "child-src"} {
		if strings.Contains(csp, absent) {
			t.Errorf("%s should not be needed; the page fetches nothing else: %s", absent, csp)
		}
	}
}

// The checkbox tick and the select chevron are SVG data URIs, and default-src 'none' refuses
// them silently: the control renders with no mark on it. data: alone is what admits them —
// it names no origin, so the page still cannot fetch an image from anywhere.
func TestCSPAdmitsInlineImagesAndNoOrigin(t *testing.T) {
	csp := cspFor(t, []string{"https://accounts.google.com"})

	if !strings.Contains(csp, "img-src data:;") {
		t.Fatalf("the tick and the chevron are data URIs: %s", csp)
	}
	if strings.Contains(csp, "img-src data: ") || strings.Contains(csp, "img-src 'self'") {
		t.Errorf("img-src should carry nothing but data:, got: %s", csp)
	}
}

func TestSecurityHeadersSetTheRest(t *testing.T) {
	h := SecurityHeaders(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

// The origins a linking flow needs. Passed to the middleware exactly as main does, so the
// consent-screen tests below also prove the redirect is added to that list rather than
// replacing it.
var providerOrigins = []string{"https://accounts.google.com", "https://accounts.zoho.eu"}

// consentChain wires what a browser actually meets: the middleware that sets the policy,
// then the authorization endpoints, with the consent screen rendered by this package. The
// operator session is injected directly, since the guard around these routes is not what is
// under test here.
func consentChain(t *testing.T, registered ...string) (http.Handler, *store.Store, user.User) {
	t.Helper()
	ctx := context.Background()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	if err := db.RegisterClient(ctx, store.Client{
		ID: "client_1", Name: "Claude", RedirectURIs: registered,
	}); err != nil {
		t.Fatal(err)
	}
	account := mail.Account{
		ID: "acct_1", Alias: "work", Address: "ada@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(ctx, me.ID, account, "sealed", ""); err != nil {
		t.Fatal(err)
	}

	oauth := oauthsrv.New(db, "https://mail.example.com")
	oauth.ConsentPage = s.ConsentPage

	asOperator := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(w, r.WithContext(user.NewContext(r.Context(), me)))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", asOperator(oauth.Authorize))
	mux.HandleFunc("POST /authorize/approve", asOperator(oauth.Approve))
	mux.HandleFunc("POST /authorize/deny", asOperator(oauth.Deny))

	return SecurityHeaders(providerOrigins, mux), db, me
}

func authorizeURL(redirect string) string {
	q := url.Values{
		"client_id":             {"client_1"},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
		"scope":                 {"read"},
		"state":                 {"opaque-state"},
	}
	return "/authorize?" + q.Encode()
}

func consentPage(t *testing.T, handler http.Handler, redirect string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, authorizeURL(redirect), nil))
	return rec
}

// The consent form posts to mailroom, which answers by redirecting the browser to the MCP
// client's callback — and `form-action` covers that whole chain, not just the post. Without
// the client's own origin in the directive the browser refuses the last hop, the client never
// receives its authorization code, and the server logs a successful 303 either way.
func TestConsentPageAdmitsTheRegisteredRedirect(t *testing.T) {
	handler, _, _ := consentChain(t, "https://claude.ai/api/mcp/auth_callback")

	rec := consentPage(t, handler, "https://claude.ai/api/mcp/auth_callback")
	if rec.Code != http.StatusOK {
		t.Fatalf("want the consent screen, got %d: %s", rec.Code, rec.Body)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://accounts.google.com https://accounts.zoho.eu https://claude.ai;") {
		t.Fatalf("the client's callback origin must join form-action, got: %s", csp)
	}
	// Linking a mailbox goes through the same policy, so the provider origins have to survive
	// a consent screen being rendered.
	for _, origin := range providerOrigins {
		if !strings.Contains(csp, origin) {
			t.Errorf("policy lost the provider origin %s: %s", origin, csp)
		}
	}
	// Only the origin: CSP stops matching paths as soon as a navigation has been redirected.
	if strings.Contains(csp, "/api/mcp") {
		t.Errorf("the directive should name an origin, not a path: %s", csp)
	}
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the rest of the policy must be untouched: %s", csp)
	}
}

// The value in the directive is the one checked against the client's registration, never the
// one on the request. Anything else would let whoever can make an operator open a link put an
// origin of their choosing into the policy of the page they are about to approve on.
func TestConsentPageRefusesAnUnregisteredRedirect(t *testing.T) {
	handler, _, _ := consentChain(t, "https://claude.ai/api/mcp/auth_callback")

	rec := consentPage(t, handler, "https://attacker.example/steal")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unregistered redirect_uri must be refused, got %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "attacker.example") {
		t.Fatalf("an unvalidated origin reached the policy: %s", csp)
	}
}

// Registering two callbacks does not put both in every policy: the directive names the one
// this authorization is actually for.
func TestConsentPageNamesOnlyTheRedirectInPlay(t *testing.T) {
	handler, _, _ := consentChain(t,
		"https://claude.ai/api/mcp/auth_callback", "https://other.example/cb")

	csp := consentPage(t, handler, "https://claude.ai/api/mcp/auth_callback").
		Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, "https://claude.ai") {
		t.Fatalf("the redirect in play should be admitted: %s", csp)
	}
	if strings.Contains(csp, "other.example") {
		t.Errorf("the client's other registered callback is not in play here: %s", csp)
	}
}

// A desktop client receives the redirect on loopback, so the port has to come along — a
// policy naming http://127.0.0.1 alone would not match http://127.0.0.1:33418.
func TestConsentPageAdmitsALoopbackRedirect(t *testing.T) {
	handler, _, _ := consentChain(t, "http://127.0.0.1:33418/callback")

	csp := consentPage(t, handler, "http://127.0.0.1:33418/callback").
		Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, " http://127.0.0.1:33418") {
		t.Fatalf("the loopback origin and its port must appear: %s", csp)
	}
}

// A native client registers a private scheme, which CSP can only express as a scheme-source.
// "cursor://anysphere.cursor-mcp" would be a host expression browsers do not agree how to
// match for a scheme the URL parser treats as opaque.
func TestConsentPageAdmitsAPrivateSchemeRedirect(t *testing.T) {
	handler, _, _ := consentChain(t, "cursor://anysphere.cursor-mcp/oauth/callback")

	csp := consentPage(t, handler, "cursor://anysphere.cursor-mcp/oauth/callback").
		Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, " cursor:;") {
		t.Fatalf("a private scheme should appear as a scheme-source: %s", csp)
	}
	if strings.Contains(csp, "cursor://") {
		t.Errorf("a scheme-source carries no host: %s", csp)
	}
}

// The whole flow, in the order a browser walks it: the consent screen, then the approval that
// redirects to the client with the code. Both responses have to carry the callback origin —
// the first because it is the document whose policy the browser enforces the submission
// against, the second because it is the response that performs the redirect.
func TestApprovalRedirectsToTheClientUnderTheSamePolicy(t *testing.T) {
	handler, _, _ := consentChain(t, "https://claude.ai/api/mcp/auth_callback")

	page := consentPage(t, handler, "https://claude.ai/api/mcp/auth_callback")
	if page.Code != http.StatusOK {
		t.Fatalf("want the consent screen, got %d: %s", page.Code, page.Body)
	}
	match := requestIDField.FindStringSubmatch(page.Body.String())
	if match == nil {
		t.Fatalf("the consent form should carry a request id: %s", page.Body)
	}

	form := url.Values{
		"request_id":   {match[1]},
		"label":        {"Claude — work triage"},
		"accounts":     {"acct_1"},
		"capabilities": {"read"},
		"expires_days": {"90"},
	}
	r := httptest.NewRequest(http.MethodPost, "/authorize/approve", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect back to the client, got %d: %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "claude.ai" || location.Query().Get("code") == "" {
		t.Fatalf("the code should go to the client's callback, got %s", rec.Header().Get("Location"))
	}
	// The hop the browser refuses is this one, so the response performing it must not be
	// served under a policy that forbids its own Location.
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "https://claude.ai") {
		t.Fatalf("the approval response should admit the callback it redirects to: %s", csp)
	}
}

// Refusing hands the client an error at the same callback, by the same redirect, so it needs
// the same treatment.
func TestDenialRedirectsToTheClientUnderTheSamePolicy(t *testing.T) {
	handler, _, _ := consentChain(t, "https://claude.ai/api/mcp/auth_callback")

	page := consentPage(t, handler, "https://claude.ai/api/mcp/auth_callback")
	match := requestIDField.FindStringSubmatch(page.Body.String())
	if match == nil {
		t.Fatalf("the consent form should carry a request id: %s", page.Body)
	}

	form := url.Values{"request_id": {match[1]}}
	r := httptest.NewRequest(http.MethodPost, "/authorize/deny", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect back to the client, got %d: %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "https://claude.ai") {
		t.Fatalf("the denial response should admit the callback it redirects to: %s", csp)
	}
}

// The middleware sets the policy before the handler runs and the handler overwrites it, which
// only works while nothing has been written. A ResponseRecorder cannot pin that — it keeps
// accepting header writes forever — so this drives it over a real connection.
func TestTheConsentPolicySurvivesARealConnection(t *testing.T) {
	handler, _, _ := consentChain(t, "https://claude.ai/api/mcp/auth_callback")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + authorizeURL("https://claude.ai/api/mcp/auth_callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want the consent screen, got %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://claude.ai") {
		t.Fatalf("the wire response should carry the widened policy: %s", csp)
	}
}
