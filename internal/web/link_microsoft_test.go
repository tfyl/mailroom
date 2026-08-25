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
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// microsoftLinking builds a server with a Microsoft client configured, and two users, because
// the interesting half of this flow is which of them a callback belongs to.
func microsoftLinking(t *testing.T) (*Server, user.User, user.User) {
	t.Helper()

	public, err := url.Parse("https://mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, db := testServerWith(t, signup.Policy{Mode: signup.Open}, &config.Config{
		PublicURL: public,
		Microsoft: config.ProviderOAuth{ClientID: "client", ClientSecret: "secret"},
		// Left empty on purpose: an instance that configures nothing must still get the
		// tenant that accepts both personal and work accounts.
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

func postMicrosoftLink(s *Server, who user.User, alias string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/accounts/link/microsoft",
		strings.NewReader(url.Values{"alias": {alias}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.linkMicrosoft(rec, r)
	return rec
}

// startMicrosoftLink drives the form post and hands back the consent URL the browser is sent
// to.
func startMicrosoftLink(t *testing.T, s *Server, who user.User, alias string) *url.URL {
	t.Helper()

	rec := postMicrosoftLink(s, who, alias)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect to Microsoft, got %d: %s", rec.Code, rec.Body)
	}
	consent, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return consent
}

// microsoftStub answers the two hosts a link touches — the identity platform's token endpoint
// and Graph — so the callback runs end to end with no network. It records what it was sent,
// since what mailroom asks for is the part these tests can establish; what Microsoft really
// answers is not.
type microsoftStub struct {
	refreshToken string // left out of the token response when empty
	refuseGraph  bool   // Graph rejects the access token it was just handed

	tokenForm  url.Values
	authHeader string
	preferHead string
}

func (m *microsoftStub) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body any) (*http.Response, error) {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
			Request:    r,
		}, nil
	}

	switch {
	case r.URL.Host == "login.microsoftonline.com" && strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		m.tokenForm = form

		token := map[string]any{
			"access_token": "graph_access",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if m.refreshToken != "" {
			token["refresh_token"] = m.refreshToken
		}
		return reply(http.StatusOK, token)

	case r.URL.Host == "graph.microsoft.com" && r.URL.Path == "/v1.0/me":
		m.authHeader = r.Header.Get("Authorization")
		m.preferHead = r.Header.Get("Prefer")
		if m.refuseGraph {
			return reply(http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"code": "InvalidAuthenticationToken"},
			})
		}
		return reply(http.StatusOK, map[string]any{
			"mail":              "ada@contoso.example",
			"userPrincipalName": "ada@contoso.onmicrosoft.example",
			"displayName":       "Ada",
		})
	}
	return nil, fmt.Errorf("the flow reached somewhere unexpected: %s %s", r.Method, r.URL)
}

func microsoftCallback(s *Server, who user.User, state string, transport http.RoundTripper) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet,
		"/accounts/link/microsoft/callback?code=xyzzy&state="+url.QueryEscape(state), nil)
	ctx := user.NewContext(r.Context(), who)
	// The context client is what both legs of the callback pick up: the token exchange and,
	// through it, the address lookup against Graph.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport})

	rec := httptest.NewRecorder()
	s.linkMicrosoftCallback(rec, r.WithContext(ctx))
	return rec
}

// The consent URL is the whole of what mailroom controls in the first leg, and three things
// about it are load-bearing: the tenant segment, which decides whether a personal Microsoft
// account may consent at all; offline_access, without which the exchange returns no refresh
// token and the mailbox works for exactly one hour; and the Graph scopes, which decide what
// the mailbox can do once it is linked.
func TestStartingAMicrosoftLinkAsksTheCommonTenantForAnOfflineGrant(t *testing.T) {
	s, ada, _ := microsoftLinking(t)

	consent := startMicrosoftLink(t, s, ada, "work")

	if consent.Host != "login.microsoftonline.com" {
		t.Errorf("consent should be granted at the identity platform, got %q", consent.Host)
	}
	// `common` and nothing else. `consumers` refuses every work or school mailbox and
	// `organizations` refuses every outlook.com one, so either would quietly halve what this
	// connector is for.
	if consent.Path != "/common/oauth2/v2.0/authorize" {
		t.Errorf("authorization path = %q, want the common tenant so both account kinds can consent", consent.Path)
	}

	q := consent.Query()
	scopes := strings.Fields(q.Get("scope"))
	held := map[string]bool{}
	for _, scope := range scopes {
		held[scope] = true
	}
	if !held["offline_access"] {
		t.Error("without offline_access Microsoft returns no refresh token and the mailbox dies in an hour")
	}
	for _, want := range []string{
		"https://graph.microsoft.com/User.Read",
		"https://graph.microsoft.com/Mail.ReadWrite",
		"https://graph.microsoft.com/Mail.Send",
		"https://graph.microsoft.com/MailboxSettings.ReadWrite",
	} {
		if !held[want] {
			t.Errorf("the consent must request %q; scope = %q", want, q.Get("scope"))
		}
	}
	if held["https://graph.microsoft.com/Mail.Read"] {
		t.Error("Mail.Read is redundant beside Mail.ReadWrite and should not be asked for")
	}

	if q.Get("prompt") != "consent" {
		t.Errorf("an authorization riding a standing consent is where a refresh token goes missing; prompt = %q", q.Get("prompt"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("response_mode") != "query" {
		t.Errorf("the callback reads the code from the query string; response_mode = %q", q.Get("response_mode"))
	}
	if q.Get("redirect_uri") != "https://mail.example.com/accounts/link/microsoft/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Error("the redirect to Microsoft carried no state")
	}
}

// An instance with no Microsoft client must refuse rather than send somebody to a consent
// screen that cannot work, and say which piece of configuration is missing.
func TestMicrosoftLinkingIsRefusedWhenNoClientIsConfigured(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	rec := postMicrosoftLink(s, users[0], "work")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("want 412 with no Microsoft client configured, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("the refusal should say Microsoft is not configured: %s", rec.Body)
	}
}

func TestMicrosoftCallbackLinksTheMailbox(t *testing.T) {
	s, ada, _ := microsoftLinking(t)
	ctx := context.Background()

	state := startMicrosoftLink(t, s, ada, "work").Query().Get("state")
	stub := &microsoftStub{refreshToken: "graph_refresh"}

	rec := microsoftCallback(s, ada, state, stub)
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
	if got.Provider != mail.ProviderMicrosoft {
		t.Errorf("provider = %q, want %q", got.Provider, mail.ProviderMicrosoft)
	}
	if got.Alias != "work" {
		t.Errorf("alias = %q", got.Alias)
	}
	// The address is Graph's answer rather than anything typed into the form, which is the
	// reason the callback talks to Graph at all.
	if got.Address != "ada@contoso.example" {
		t.Errorf("address = %q, want the address Microsoft reported", got.Address)
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
	if opened != "graph_refresh" {
		t.Errorf("stored credential = %q, want the refresh token", opened)
	}

	// The identity platform documents the client credentials as form parameters. Left to
	// probe, the library spends a refused Basic-auth round trip before every exchange and
	// every refresh.
	if stub.tokenForm.Get("client_id") != "client" || stub.tokenForm.Get("client_secret") != "secret" {
		t.Errorf("the token request should carry the client credentials as parameters, got %v", stub.tokenForm)
	}
	if stub.authHeader != "Bearer graph_access" {
		t.Errorf("Authorization = %q, want the Bearer scheme Graph takes", stub.authHeader)
	}
	if stub.preferHead != `IdType="ImmutableId"` {
		t.Errorf("Prefer = %q, want immutable ids on every Graph request", stub.preferHead)
	}
}

// A mailbox linked without a refresh token works until the access token expires an hour later
// and then fails as a credential error with nothing to point at. Refusing at the link is the
// only place the cause is still visible — and the cause is nearly always an app registration
// missing offline_access, so the refusal says so.
func TestMicrosoftCallbackRefusesAGrantWithNoRefreshToken(t *testing.T) {
	s, ada, _ := microsoftLinking(t)

	state := startMicrosoftLink(t, s, ada, "work").Query().Get("state")

	rec := microsoftCallback(s, ada, state, &microsoftStub{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when Microsoft returns no refresh token, got %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "refresh token") {
		t.Errorf("the refusal should name what was missing: %s", body)
	}
	if !strings.Contains(body, "offline_access") {
		t.Errorf("the refusal should name the permission that produces one: %s", body)
	}

	accounts, err := s.store.ListAccounts(context.Background(), ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a mailbox that cannot refresh must not be stored, got %+v", accounts)
	}
}

// The same attack the Google and Zoho callbacks bind against: someone completes Microsoft's
// consent for a mailbox they own, does not follow the redirect, and gets a signed-in victim to
// open the callback URL. It is a top-level GET, so the victim's session rides along.
func TestMicrosoftLinkCallbackRefusesTheUserWhoDidNotStartIt(t *testing.T) {
	s, ada, mallory := microsoftLinking(t)

	state := startMicrosoftLink(t, s, mallory, "inbox").Query().Get("state")

	rec := microsoftCallback(s, ada, state, &microsoftStub{refreshToken: "graph_refresh"})
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

// A state that was never issued, one already spent and one belonging to somebody else all have
// to be refused the same way. Anything finer tells a caller which states are live.
func TestMicrosoftLinkCallbackRefusesAnUnknownOrSpentState(t *testing.T) {
	s, ada, mallory := microsoftLinking(t)
	stub := &microsoftStub{refreshToken: "graph_refresh"}

	invented := microsoftCallback(s, ada, "link_never_issued", stub)
	if invented.Code != http.StatusBadRequest {
		t.Fatalf("an invented state must be refused, got %d: %s", invented.Code, invented.Body)
	}

	state := startMicrosoftLink(t, s, ada, "work").Query().Get("state")
	if rec := microsoftCallback(s, ada, state, stub); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first callback should have linked, got %d: %s", rec.Code, rec.Body)
	}
	spent := microsoftCallback(s, ada, state, stub)
	if spent.Code != http.StatusBadRequest {
		t.Fatalf("a replayed state must be refused, got %d: %s", spent.Code, spent.Body)
	}

	somebodyElses := microsoftCallback(s, ada,
		startMicrosoftLink(t, s, mallory, "inbox").Query().Get("state"), stub)
	if somebodyElses.Body.String() != invented.Body.String() || somebodyElses.Body.String() != spent.Body.String() {
		t.Errorf("the three refusals differ:\n%q\n%q\n%q",
			invented.Body.String(), spent.Body.String(), somebodyElses.Body.String())
	}
}

// Graph refusing the token Microsoft issued seconds ago is not a mailbox that needs
// re-linking, and the provider's words for it — "re-link required" — would send somebody
// looking for a mailbox that was never linked in the first place. The likely cause is an app
// registration whose API permissions do not include what was asked for.
func TestMicrosoftCallbackExplainsAGraphRefusalOfAFreshToken(t *testing.T) {
	s, ada, _ := microsoftLinking(t)

	state := startMicrosoftLink(t, s, ada, "work").Query().Get("state")

	rec := microsoftCallback(s, ada, state, &microsoftStub{refreshToken: "graph_refresh", refuseGraph: true})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when Graph refuses the token, got %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "app registration") {
		t.Errorf("the refusal should point at the app registration, which is the likely cause: %s", body)
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

// The button is offered only where it can work, and the page says what is missing where it
// cannot — an instance with no Microsoft client should not show a control that only produces
// a 412.
func TestTheMailboxesPageOffersMicrosoftOnlyWhenItIsConfigured(t *testing.T) {
	configured, ada, _ := microsoftLinking(t)
	body := renderAccountsPage(t, configured, ada)
	if !strings.Contains(body, `action="/accounts/link/microsoft"`) {
		t.Errorf("the page should carry the Microsoft form: %s", body)
	}
	if !strings.Contains(body, "Link Microsoft mailbox</button>") {
		t.Errorf("the page should offer the Microsoft button: %s", body)
	}
	if strings.Contains(body, "disabled>Link Microsoft mailbox") {
		t.Errorf("a configured instance must not disable the Microsoft button: %s", body)
	}

	bare, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(bare, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body = renderAccountsPage(t, bare, users[0])
	if !strings.Contains(body, "disabled>Link Microsoft mailbox") {
		t.Errorf("an unconfigured instance must disable the Microsoft button: %s", body)
	}
	if !strings.Contains(body, "MAILROOM_MICROSOFT_CLIENT_ID") {
		t.Errorf("the page should say which configuration is missing: %s", body)
	}
}

func renderAccountsPage(t *testing.T, s *Server, who user.User) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	s.accounts(rec, r.WithContext(user.NewContext(r.Context(), who)))
	return rec.Body.String()
}
