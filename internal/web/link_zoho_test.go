package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/zoho"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// zohoRegion is deliberately not the default. Zoho partitions accounts by data centre and
// every host in the flow follows the configured one, so a region hardcoded anywhere shows up
// here as a request to somewhere these tests refuse to answer.
const zohoRegion = "eu"

// zohoLinking builds a server with a Zoho client configured, and two users, because the
// interesting half of this flow is which of them a callback belongs to.
func zohoLinking(t *testing.T) (*Server, user.User, user.User) {
	t.Helper()

	public, err := url.Parse("https://mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, db := testServerWith(t, signup.Policy{Mode: signup.Open}, &config.Config{
		PublicURL:  public,
		Zoho:       config.ProviderOAuth{ClientID: "client", ClientSecret: "secret"},
		ZohoRegion: zohoRegion,
	})

	signInAs(s, "ada", "")
	signInAs(s, "mallory", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	find := func(subject string) user.User {
		for _, u := range users {
			if u.Subject == subject {
				return u
			}
		}
		t.Fatalf("no user with subject %q", subject)
		return user.User{}
	}
	return s, find("ada"), find("mallory")
}

// startZohoLink drives the form post and hands back the consent URL the browser is sent to.
func startZohoLink(t *testing.T, s *Server, who user.User, alias string) *url.URL {
	t.Helper()

	rec := postZohoLink(s, who, alias)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect to Zoho, got %d: %s", rec.Code, rec.Body)
	}
	consent, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return consent
}

func postZohoLink(s *Server, who user.User, alias string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/accounts/link/zoho",
		strings.NewReader(url.Values{"alias": {alias}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.linkZoho(rec, r)
	return rec
}

// zohoStub answers the two hosts a link touches — the token endpoint at
// accounts.zoho.<region> and the mailbox at mail.zoho.<region> — so the callback runs end to
// end with no network. It records what it was sent, since what mailroom asks for is the part
// these tests can establish; what Zoho really answers is not.
type zohoStub struct {
	refreshToken  string // left out of the token response when empty
	refuseMailbox bool   // the mailbox host rejects the access token

	tokenForm  url.Values
	authHeader string
}

func (z *zohoStub) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(body any) (*http.Response, error) {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
			Request:    r,
		}, nil
	}

	switch {
	case r.URL.Host == "accounts.zoho."+zohoRegion && r.URL.Path == "/oauth/v2/token":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		z.tokenForm = form

		token := map[string]any{
			"access_token": "zoho_access",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if z.refreshToken != "" {
			token["refresh_token"] = z.refreshToken
		}
		return reply(token)

	case r.URL.Host == "mail.zoho."+zohoRegion && r.URL.Path == "/api/accounts":
		z.authHeader = r.Header.Get("Authorization")
		if z.refuseMailbox {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"status":{"code":401}}`)),
				Request:    r,
			}, nil
		}
		return reply(map[string]any{
			"status": map[string]any{"code": 200, "description": "success"},
			"data": []map[string]any{
				{"accountId": "9000", "primaryEmailAddress": "ada@zoho.example"},
			},
		})
	}
	return nil, fmt.Errorf("the flow reached somewhere unexpected: %s %s", r.Method, r.URL)
}

func zohoCallback(s *Server, who user.User, state string, transport http.RoundTripper) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet,
		"/accounts/link/zoho/callback?code=xyzzy&state="+url.QueryEscape(state), nil)
	ctx := user.NewContext(r.Context(), who)
	// The context client is what both legs of the callback pick up: the token exchange and,
	// through it, the address lookup against the mailbox host.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport})

	rec := httptest.NewRecorder()
	s.linkZohoCallback(rec, r.WithContext(ctx))
	return rec
}

// The consent URL is the whole of what mailroom controls in the first leg, and three separate
// things about it are load-bearing: the host follows the configured data centre, the scopes
// are comma-separated because Zoho reads a space-separated list as one unknown scope, and the
// offline grant is asked for with the consent screen forced — which is the only combination
// that returns a refresh token.
func TestStartingAZohoLinkAsksTheConfiguredDataCentreForAnOfflineGrant(t *testing.T) {
	s, ada, _ := zohoLinking(t)

	consent := startZohoLink(t, s, ada, "work")

	if consent.Host != "accounts.zoho."+zohoRegion {
		t.Errorf("consent should be granted at the configured data centre, got %q", consent.Host)
	}
	if consent.Path != "/oauth/v2/auth" {
		t.Errorf("authorization path = %q", consent.Path)
	}

	q := consent.Query()
	if want := strings.Join(zoho.Scopes, ","); q.Get("scope") != want {
		t.Errorf("Zoho separates scopes with commas, not spaces:\n want %q\n  got %q", want, q.Get("scope"))
	}
	if q.Get("access_type") != "offline" {
		t.Errorf("without an offline grant Zoho returns no refresh token; access_type = %q", q.Get("access_type"))
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("without a forced consent Zoho reuses a standing one and returns no refresh token; prompt = %q", q.Get("prompt"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("redirect_uri") != "https://mail.example.com/accounts/link/zoho/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Error("the redirect to Zoho carried no state")
	}
}

// An instance with no Zoho client must refuse rather than send somebody to a consent screen
// that cannot work, and say which piece of configuration is missing.
func TestZohoLinkingIsRefusedWhenNoClientIsConfigured(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	rec := postZohoLink(s, users[0], "work")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("want 412 with no Zoho client configured, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("the refusal should say Zoho is not configured: %s", rec.Body)
	}
}

func TestZohoCallbackLinksTheMailbox(t *testing.T) {
	s, ada, _ := zohoLinking(t)
	ctx := context.Background()

	state := startZohoLink(t, s, ada, "work").Query().Get("state")
	stub := &zohoStub{refreshToken: "zoho_refresh"}

	rec := zohoCallback(s, ada, state, stub)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after linking, got %d: %s", rec.Code, rec.Body)
	}

	accounts, err := s.store.ListAccounts(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("want one linked mailbox, got %+v", accounts)
	}
	got := accounts[0]
	if got.Provider != mail.ProviderZoho {
		t.Errorf("provider = %q, want %q", got.Provider, mail.ProviderZoho)
	}
	if got.Alias != "work" {
		t.Errorf("alias = %q", got.Alias)
	}
	// The address is Zoho's answer rather than anything typed into the form, which is the
	// reason the callback talks to the mailbox host at all.
	if got.Address != "ada@zoho.example" {
		t.Errorf("address = %q, want the address Zoho reported", got.Address)
	}

	// What the mailbox is worth is whether the refresh token comes back out again.
	sealed, err := s.store.Credential(ctx, ada.ID, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := s.sealer.OpenString(sealed, string(got.ID))
	if err != nil {
		t.Fatalf("the credential does not open with the account id as context: %v", err)
	}
	if opened != "zoho_refresh" {
		t.Errorf("stored credential = %q, want the refresh token", opened)
	}

	// Zoho wants its client credentials as request parameters. Left to probe, the library
	// spends a refused Basic-auth round trip before every exchange and every refresh.
	if stub.tokenForm.Get("client_id") != "client" || stub.tokenForm.Get("client_secret") != "secret" {
		t.Errorf("the token request should carry the client credentials as parameters, got %v", stub.tokenForm)
	}
	// And it refuses the Bearer scheme its own token response names.
	if stub.authHeader != "Zoho-oauthtoken zoho_access" {
		t.Errorf("Authorization = %q, want the Zoho-oauthtoken scheme", stub.authHeader)
	}
}

// A mailbox linked without a refresh token works until the access token expires an hour
// later and then fails as a credential error with nothing to point at. Refusing at the link
// is the only place the cause is still visible.
func TestZohoCallbackRefusesAGrantWithNoRefreshToken(t *testing.T) {
	s, ada, _ := zohoLinking(t)

	state := startZohoLink(t, s, ada, "work").Query().Get("state")

	rec := zohoCallback(s, ada, state, &zohoStub{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when Zoho returns no refresh token, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "refresh token") {
		t.Errorf("the refusal should name what was missing: %s", rec.Body)
	}

	accounts, err := s.store.ListAccounts(context.Background(), ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a mailbox that cannot refresh must not be stored, got %+v", accounts)
	}
}

// The same attack the Google callback binds against: someone completes Zoho's consent for a
// mailbox they own, does not follow the redirect, and gets a signed-in victim to open the
// callback URL. It is a top-level GET, so the victim's session rides along.
func TestZohoLinkCallbackRefusesTheUserWhoDidNotStartIt(t *testing.T) {
	s, ada, mallory := zohoLinking(t)

	state := startZohoLink(t, s, mallory, "inbox").Query().Get("state")

	rec := zohoCallback(s, ada, state, &zohoStub{refreshToken: "zoho_refresh"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a callback belonging to another user must be refused, got %d: %s", rec.Code, rec.Body)
	}
	accounts, err := s.store.ListAccounts(context.Background(), ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("nothing may be linked into the victim's account, got %+v", accounts)
	}
	if _, ok := s.links.take(state, mallory.ID); ok {
		t.Error("the refused attempt is still claimable; a second victim could complete it")
	}
}

// A state that was never issued, one already spent and one belonging to somebody else all
// have to be refused the same way. Anything finer tells a caller which states are live.
func TestZohoLinkCallbackRefusesAnUnknownOrSpentState(t *testing.T) {
	s, ada, mallory := zohoLinking(t)
	stub := &zohoStub{refreshToken: "zoho_refresh"}

	invented := zohoCallback(s, ada, "link_never_issued", stub)
	if invented.Code != http.StatusBadRequest {
		t.Fatalf("an invented state must be refused, got %d: %s", invented.Code, invented.Body)
	}

	state := startZohoLink(t, s, ada, "work").Query().Get("state")
	if rec := zohoCallback(s, ada, state, stub); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first callback should have linked, got %d: %s", rec.Code, rec.Body)
	}
	spent := zohoCallback(s, ada, state, stub)
	if spent.Code != http.StatusBadRequest {
		t.Fatalf("a replayed state must be refused, got %d: %s", spent.Code, spent.Body)
	}

	somebodyElses := zohoCallback(s, ada, startZohoLink(t, s, mallory, "inbox").Query().Get("state"), stub)
	if somebodyElses.Body.String() != invented.Body.String() || somebodyElses.Body.String() != spent.Body.String() {
		t.Errorf("the three refusals differ:\n%q\n%q\n%q",
			invented.Body.String(), spent.Body.String(), somebodyElses.Body.String())
	}
}

// The button is offered only where it can work, and the page says what is missing where it
// cannot — an instance with no Zoho client should not show a control that only produces a 412.
func TestTheMailboxesPageOffersZohoOnlyWhenItIsConfigured(t *testing.T) {
	render := func(s *Server, who user.User) string {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/accounts", nil)
		s.accounts(rec, r.WithContext(user.NewContext(r.Context(), who)))
		return rec.Body.String()
	}

	configured, ada, _ := zohoLinking(t)
	body := render(configured, ada)
	if !strings.Contains(body, `action="/accounts/link/zoho"`) {
		t.Errorf("the page should carry the Zoho form: %s", body)
	}
	if strings.Contains(body, "Link Zoho mailbox</button>") == false {
		t.Errorf("the page should offer the Zoho button: %s", body)
	}
	if strings.Contains(body, "disabled>Link Zoho mailbox") {
		t.Errorf("a configured instance must not disable the Zoho button: %s", body)
	}

	bare, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(bare, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body = render(bare, users[0])
	if !strings.Contains(body, "disabled>Link Zoho mailbox") {
		t.Errorf("an unconfigured instance must disable the Zoho button: %s", body)
	}
	if !strings.Contains(body, "MAILROOM_ZOHO_CLIENT_ID") {
		t.Errorf("the page should say which configuration is missing: %s", body)
	}
}

// The likeliest real failure: a client and a mailbox in different Zoho data centres. The
// mailbox host refuses a token that was issued seconds ago, and the provider calls that
// "re-link required" — which is true of a mailbox that was working and stopped, and sends
// somebody looking for a mailbox that was never linked in the first place.
func TestZohoCallbackExplainsAMailboxTheDataCentreRefuses(t *testing.T) {
	s, ada, _ := zohoLinking(t)

	state := startZohoLink(t, s, ada, "work").Query().Get("state")

	rec := zohoCallback(s, ada, state, &zohoStub{refreshToken: "zoho_refresh", refuseMailbox: true})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when the mailbox host refuses the token, got %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data centre") {
		t.Errorf("the refusal should point at the region, which is the likely cause: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "re-link") {
		t.Errorf("a first attempt must not be described as needing a re-link: %s", body)
	}

	accounts, err := s.store.ListAccounts(context.Background(), ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a mailbox that could not be read must not be stored, got %+v", accounts)
	}
}
