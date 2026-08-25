package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// The select-all controls are a round trip through the server, so the only test that proves
// anything about them runs the real handlers against the real template. A stub of either
// would be testing the stub: the whole risk in this feature is what the second submission
// does to state the first one left behind.

const (
	consentRedirect = "http://127.0.0.1:9999/cb"
	consentVerifier = "a-verifier-long-enough-to-be-a-real-one"
)

type consentRig struct {
	oauth *oauthsrv.Server
	db    *store.Store
	ada   user.User
	bob   user.User
}

func newConsentRig(t *testing.T) consentRig {
	t.Helper()
	ctx := context.Background()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	signInAs(s, "bob", "")

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rig := consentRig{oauth: oauthsrv.New(db, "https://mail.example.com"), db: db}
	rig.oauth.ConsentPage = s.ConsentPage
	for _, u := range users {
		switch u.Subject {
		case "ada":
			rig.ada = u
		case "bob":
			rig.bob = u
		}
	}
	if rig.ada.ID == "" || rig.bob.ID == "" {
		t.Fatalf("both users should exist, got %+v", users)
	}

	link := func(owner user.User, id, alias string) {
		t.Helper()
		err := db.LinkAccount(ctx, owner.ID, mail.Account{
			ID: mail.AccountID(id), Alias: alias, Address: alias + "@example.com",
			Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
		}, "sealed", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	link(rig.ada, "acct_ada_work", "ada-work")
	link(rig.ada, "acct_ada_home", "ada-home")
	link(rig.bob, "acct_bob_work", "bob-work")

	err = db.RegisterClient(ctx, store.Client{
		ID: "client_1", Name: "An agent", RedirectURIs: []string{consentRedirect},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rig
}

var requestIDField = regexp.MustCompile(`name="request_id" value="([^"]+)"`)

// open drives GET /authorize the way a browser arriving from a client would, and returns the
// rendered page along with the request id the form will submit back.
func (rig consentRig) open(t *testing.T, as user.User) (string, string) {
	t.Helper()
	return rig.openWithScope(t, as, "read")
}

// openWithScope is the same, for the tests that care what the client asked for.
func (rig consentRig) openWithScope(t *testing.T, as user.User, scope string) (string, string) {
	t.Helper()
	return rig.openClient(t, as, "client_1", consentRedirect, scope)
}

// register introduces a client the way anything on the internet may: an unauthenticated POST
// to the registration endpoint with a name and a callback of its own choosing. Driven through
// the real route rather than written straight into the store, so what these tests read on the
// consent screen is what an open registration actually puts there.
func (rig consentRig) register(t *testing.T, name, redirect string) string {
	t.Helper()

	mux := http.NewServeMux()
	rig.oauth.Routes(mux)
	body, err := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{redirect}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /register: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.ClientID == "" {
		t.Fatalf("registration returned no client_id: %s", rec.Body.String())
	}
	return registered.ClientID
}

// openClient is openWithScope for a client the test registered itself.
func (rig consentRig) openClient(t *testing.T, as user.User, clientID, redirect, scope string) (string, string) {
	t.Helper()

	sum := sha256.Sum256([]byte(consentVerifier))
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"scope":                 {scope},
	}
	r := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.oauth.Authorize(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /authorize: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	m := requestIDField.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the consent page carries no request_id: %s", body)
	}
	return body, m[1]
}

func (rig consentRig) post(t *testing.T, h http.HandlerFunc, path string, as user.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// checkbox finds one checkbox on the rendered page and returns its whole tag. It reads the
// markup the browser would rather than trusting that a value appearing somewhere on the page
// means the box next to it is checked — and it finds the input by its name and value rather
// than by the order its attributes happen to be written in, because the attributes are a
// question of how the template is laid out and the identity of the box is not.
func checkbox(body, name, value string) (string, bool) {
	for _, tag := range regexp.MustCompile(`<input[^>]*>`).FindAllString(body, -1) {
		got, _ := attribute(tag, "name")
		if got != name {
			continue
		}
		if got, _ := attribute(tag, "value"); got == value {
			return tag, true
		}
	}
	return "", false
}

func ticked(t *testing.T, body, name, value string) bool {
	t.Helper()
	tag, ok := checkbox(body, name, value)
	if !ok {
		t.Fatalf("no %s checkbox for %q on the page", name, value)
	}
	return regexp.MustCompile(`\schecked\b`).MatchString(tag)
}

func hasCheckbox(body, name, value string) bool {
	_, ok := checkbox(body, name, value)
	return ok
}

func TestSelectAllCapabilitiesKeepsTheMailboxesAlreadyTicked(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Work triage"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read"},
		"expires_days": {"365"},
		"reselect":     {"all-capabilities"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, c := range mail.AllCapabilities {
		if !ticked(t, body, "capabilities", string(c)) {
			t.Errorf("select all should have ticked %s", c)
		}
	}
	if !ticked(t, body, "accounts", "acct_ada_work") {
		t.Error("selecting all capabilities must not clear a mailbox the operator had ticked")
	}
	if ticked(t, body, "accounts", "acct_ada_home") {
		t.Error("a mailbox the operator left alone must stay unticked")
	}
	if !strings.Contains(body, `name="label" value="Work triage"`) {
		t.Errorf("the grant name should survive the round trip: %s", body)
	}
	if !strings.Contains(body, `<option value="365" selected>`) {
		t.Errorf("the chosen expiry should survive the round trip: %s", body)
	}
}

func TestSelectAllMailboxesKeepsTheCapabilitiesAlreadyTicked(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Work triage"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "draft"},
		"expires_days": {"90"},
		"reselect":     {"all-mailboxes"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, id := range []string{"acct_ada_work", "acct_ada_home"} {
		if !ticked(t, body, "accounts", id) {
			t.Errorf("select all should have ticked %s", id)
		}
	}
	for _, c := range []mail.Capability{mail.CapRead, mail.CapDraft} {
		if !ticked(t, body, "capabilities", string(c)) {
			t.Errorf("selecting all mailboxes must not clear %s", c)
		}
	}
	for _, c := range []mail.Capability{mail.CapSend, mail.CapDestructive} {
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("%s was never ticked and must not appear ticked", c)
		}
	}
}

// Deselect is the way back from a mis-click, and it is the only direction on this page that
// can only ever narrow a grant.
func TestDeselectAllClearsOneGroupAndLeavesTheOther(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id":   {requestID},
		"accounts":     {"acct_ada_work", "acct_ada_home"},
		"capabilities": {"read", "send"},
		"reselect":     {"no-capabilities"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, c := range mail.AllCapabilities {
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("deselect all should have cleared %s", c)
		}
	}
	for _, id := range []string{"acct_ada_work", "acct_ada_home"} {
		if !ticked(t, body, "accounts", id) {
			t.Errorf("clearing the capabilities must not clear %s", id)
		}
	}
}

// The one that matters: a select-all must leave the pending authorization request where it
// was, or the approval that follows it is answered with "this authorization request expired".
func TestApprovingStillWorksAfterASelectAll(t *testing.T) {
	rig := newConsentRig(t)
	ctx := context.Background()
	_, requestID := rig.open(t, rig.ada)

	reselected := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Work triage"},
		"accounts":     {"acct_ada_work"},
		"expires_days": {"90"},
		"reselect":     {"all-capabilities"},
	})
	if reselected.Code != http.StatusOK {
		t.Fatalf("reselect: want 200, got %d: %s", reselected.Code, reselected.Body.String())
	}

	approve := url.Values{
		"request_id":   {requestID},
		"label":        {"Work triage"},
		"accounts":     {"acct_ada_work"},
		"expires_days": {"90"},
	}
	for _, c := range mail.AllCapabilities {
		approve.Add("capabilities", string(c))
	}
	rec := rig.post(t, rig.oauth.Approve, "/authorize/approve", rig.ada, approve)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve after a select-all: want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no authorization code in %s", rec.Header().Get("Location"))
	}

	grants, err := rig.db.ListGrants(ctx, rig.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("want exactly one grant, got %d", len(grants))
	}
	g := grants[0]
	if got, want := len(g.Accounts), 1; got != want || g.Accounts[0] != "acct_ada_work" {
		t.Errorf("want only acct_ada_work on the grant, got %v", g.Accounts)
	}
	if got, want := g.Caps.String(), mail.NewSet(mail.AllCapabilities...).String(); got != want {
		t.Errorf("want scopes %q, got %q", want, got)
	}

	// Redeem the code as well. The grant existing proves Approve ran; redeeming proves the
	// code it issued is the usable one, which is what the client is waiting for.
	mux := http.NewServeMux()
	rig.oauth.Routes(mux)
	token := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {"client_1"},
		"code_verifier": {consentVerifier},
		"redirect_uri":  {consentRedirect},
	}.Encode()))
	token.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRec := httptest.NewRecorder()
	mux.ServeHTTP(tokenRec, token)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("token exchange: want 200, got %d: %s", tokenRec.Code, tokenRec.Body.String())
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(tokenRec.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}
	if issued.AccessToken == "" {
		t.Error("no access token issued")
	}
	if want := mail.NewSet(mail.AllCapabilities...).String(); issued.Scope != want {
		t.Errorf("want scope %q, got %q", want, issued.Scope)
	}
}

// Ownership is the property the whole store is built around, and a select-all is a new way
// to name a mailbox id in a form. Neither ticking every box nor carrying a submitted one
// forward may reach a mailbox belonging to somebody else.
func TestSelectAllCannotReachAnotherUsersMailbox(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	for _, tc := range []struct {
		name     string
		reselect string
	}{
		{"ticking every mailbox", "all-mailboxes"},
		{"carrying the submitted ones forward", "all-capabilities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
				"request_id": {requestID},
				// Forged: bob's mailbox is not on ada's consent screen, so a browser could
				// not have submitted this.
				"accounts": {"acct_ada_work", "acct_bob_work"},
				"reselect": {tc.reselect},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()

			if strings.Contains(body, "acct_bob_work") || strings.Contains(body, "bob-work") {
				t.Errorf("another user's mailbox reached ada's consent screen: %s", body)
			}
			if hasCheckbox(body, "accounts", "acct_bob_work") {
				t.Error("another user's mailbox was offered as a box to tick")
			}
			for _, id := range []string{"acct_ada_work", "acct_ada_home"} {
				if !hasCheckbox(body, "accounts", id) {
					t.Errorf("ada's own mailbox %s should still be on the page", id)
				}
			}
		})
	}
}

// A request id belonging to another session is refused before anything is rendered, the same
// way Deny refuses it. Otherwise a leaked id would let one user redraw another's consent
// screen, and the page would name mailboxes to whoever asked.
func TestReselectRefusesAnotherSessionsRequest(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.bob, url.Values{
		"request_id": {requestID},
		"reselect":   {"all-mailboxes"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// And ada's own request is still there to be approved: refusing bob must not consume it.
	if rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id": {requestID},
		"reselect":   {"all-mailboxes"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("ada's request should have survived bob's attempt, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A consent screen that arrives with boxes already ticked is a formality rather than a
// decision, so the first render must tick nothing at all — including the scope the client
// asked for, which this request carries.
func TestAFreshConsentScreenTicksNothing(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	for _, c := range mail.AllCapabilities {
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("%s is ticked on a fresh consent screen", c)
		}
	}
	for _, id := range []string{"acct_ada_work", "acct_ada_home"} {
		if ticked(t, body, "accounts", id) {
			t.Errorf("%s is ticked on a fresh consent screen", id)
		}
	}
	// The expiry default is drawn by the handler now rather than by the markup, so it is
	// worth proving it is still the same default.
	if !strings.Contains(body, `<option value="90" selected>`) {
		t.Errorf("90 days should still be the default expiry: %s", body)
	}
}

func TestReselectRefusesAnUnknownSelection(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id": {requestID},
		"reselect":   {"all-the-things"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The consent screen is one form, so it carries one CSRF field, before and after a round
// trip. The select-all buttons submit that same form through formaction rather than opening
// a second one, which is what keeps this true.
func TestTheConsentFormCarriesItsCSRFFieldAfterASelectAll(t *testing.T) {
	rig := newConsentRig(t)
	first, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id": {requestID},
		"reselect":   {"all-capabilities"},
	})
	for name, body := range map[string]string{"first render": first, "after select all": rec.Body.String()} {
		forms := strings.Count(body, "<form")
		if forms == 0 {
			t.Errorf("%s: rendered no forms at all, so this proves nothing", name)
		}
		if fields := strings.Count(body, `name="csrf_token"`); forms != fields {
			t.Errorf("%s: every form needs a csrf_token field: %d forms, %d fields", name, forms, fields)
		}
	}
}

// Pressing Enter in the grant-name field submits through the form's first submit button.
// That used to be Approve; now that there are select-all buttons above it, it is one of
// those, so the first one has to be a deselect. An Enter that quietly ticked every mailbox
// is the shape of accident this screen exists to prevent.
func TestTheFirstSubmitButtonOnlyEverNarrowsTheSelection(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	form := body[strings.Index(body, "<form"):]
	first := regexp.MustCompile(`<button[^>]*>`).FindString(form)
	if first == "" {
		t.Fatalf("the consent form has no buttons at all: %s", body)
	}
	if !strings.Contains(first, `value="no-mailboxes"`) {
		t.Errorf("the form's default submit button must be one that only clears ticks, got %s", first)
	}
}

// --- Where the code is going ---
//
// The scenario these are for: registration is open, so anybody may register a client called
// "Claude" whose callback is theirs, and then send the operator a link to /authorize. The
// operator sees a page headed "Authorize Claude", ticks read and send on their own mailbox,
// and the code goes to the registrant. Until the screen named the callback there was nothing
// on it that told that apart from the real thing — the destination existed only in the pending
// request and in a CSP header nobody reads.

var redirectOriginTag = regexp.MustCompile(`<span[^>]*\sdata-redirect-origin[^>]*>([^<]*)</span>`)

// originsOn returns what each origin element on the page actually says, in order.
func originsOn(body string) []string {
	var out []string
	for _, m := range redirectOriginTag.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

func TestTheConsentScreenNamesWhereTheCodeWillBeSent(t *testing.T) {
	rig := newConsentRig(t)
	id := rig.register(t, "Claude", "https://evil.example/cb")
	body, _ := rig.openClient(t, rig.ada, id, "https://evil.example/cb", "read send")

	origins := originsOn(body)
	if len(origins) == 0 {
		t.Fatalf("the consent screen never says where the code goes: %s", body)
	}
	for _, got := range origins {
		if got != "https://evil.example" {
			t.Errorf("the screen names %q as the destination, want %q", got, "https://evil.example")
		}
	}

	// Once at the top is not enough on a page this long: the notice somebody read before
	// scrolling is not what they are looking at when they press Approve. So it has to be
	// stated where the decision is made too, and the summary the script writes is the marker
	// for that part of the page.
	summary := strings.Index(body, "data-consent-summary")
	if summary < 0 {
		t.Fatalf("the decision area is not where this test thinks it is: %s", body)
	}
	if len(redirectOriginTag.FindAllString(body[:summary], -1)) == 0 {
		t.Error("the destination is not stated above the form")
	}
	if len(redirectOriginTag.FindAllString(body[summary:], -1)) == 0 {
		t.Error("the destination is not stated beside the Approve button")
	}
}

// A remote host has to be recognised by the operator and a loopback one does not, so they are
// worded differently — and a screen that carried the same alarm either way would be alarming
// about being a consent screen. Both still say the address.
func TestALoopbackCallbackIsNamedAsWhatItIs(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	origins := originsOn(body)
	if len(origins) == 0 {
		t.Fatalf("the consent screen never says where the code goes: %s", body)
	}
	for _, got := range origins {
		if got != "http://127.0.0.1:9999" {
			t.Errorf("the screen names %q as the destination, want %q", got, "http://127.0.0.1:9999")
		}
	}
	if !strings.Contains(body, "a program on this computer") {
		t.Errorf("a loopback callback should be described as one: %s", body)
	}
}

// The destination survives a select-all, and is still read from the pending request rather
// than from the form that was just submitted.
func TestTheDestinationIsStillNamedAfterASelectAll(t *testing.T) {
	rig := newConsentRig(t)
	id := rig.register(t, "Claude", "https://evil.example/cb")
	_, requestID := rig.openClient(t, rig.ada, id, "https://evil.example/cb", "read")

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id": {requestID},
		"reselect":   {"all-capabilities"},
		// A submission cannot choose the destination that is drawn back at it.
		"redirect_uri": {"https://claude.ai/cb"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	origins := originsOn(rec.Body.String())
	if len(origins) == 0 {
		t.Fatalf("the redrawn screen says nothing about the destination: %s", rec.Body.String())
	}
	for _, got := range origins {
		if got != "https://evil.example" {
			t.Errorf("after a select-all the screen names %q, want %q", got, "https://evil.example")
		}
	}
}

// The whole value of the line is that it is a fact rather than a claim, so it has to come out
// of the redirect URI and out of nothing the client is free to write. A name is free text an
// open registration chose. A path and a query are free text on the same registration, and are
// unbounded where the name is capped — which is why only the origin is shown.
func TestTheStatedOriginCannotBeSpoofedByANameOrAPath(t *testing.T) {
	const truthful = "https://evil.example"
	padding := strings.Repeat("claude.ai/", 30)
	redirect := "https://evil.example/cb/" + padding + "?client=https://claude.ai"

	for _, name := range []string{
		// Prose that reads as a destination.
		"Claude — callback https://claude.ai",
		// Markup that would be one, if the page were assembled by pasting rather than by
		// html/template.
		`<span data-redirect-origin>https://claude.ai</span>`,
		// A name that would end the attribute it lands in.
		`Claude" data-redirect-origin="https://claude.ai`,
	} {
		rig := newConsentRig(t)
		id := rig.register(t, name, redirect)
		body, _ := rig.openClient(t, rig.ada, id, redirect, "read")

		origins := originsOn(body)
		if len(origins) == 0 {
			t.Fatalf("%q: the consent screen names no destination: %s", name, body)
		}
		for _, got := range origins {
			if got != truthful {
				t.Errorf("a client named %q got %q read back as its destination, want %q", name, got, truthful)
			}
		}
		// The name is not censored — it is on the page, escaped, where a name belongs. Without
		// this the test would pass just as well against a page that dropped it, which would be
		// a different bug and not this fix.
		if !strings.Contains(body, "claude.ai") {
			t.Errorf("%q: the client's own name should still be rendered somewhere: %s", name, body)
		}
		if strings.Contains(body, "<span data-redirect-origin>https://claude.ai") {
			t.Errorf("%q: the client name reached the page as markup", name)
		}
		// The padding never appears at all: an origin cannot be pushed out of sight by a path
		// that is not rendered.
		if strings.Contains(body, padding) {
			t.Errorf("%q: the redirect's path is on the page, so the origin can be padded away", name)
		}
	}
}

// Prints the consent screen so the markup around the new controls can be read rather than
// inferred from assertions.
func TestConsentPageMarkup(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)
	t.Log("\n" + body)
}

// --- What the script may and may not change ---
//
// The select-all controls are enhanced by internal/web/static/app.js, which ticks the boxes in
// the browser instead of asking the server to redraw the page. The enhancement is allowed to
// be faster; it is not allowed to be the only way the control works, and it is not allowed to
// mean something the server does not. The tests above already drive the no-script path through
// the real handlers. These three keep the markup that path depends on from being enhanced away.

var reselectButton = regexp.MustCompile(`<button([^>]*name="reselect"[^>]*)>`)

func attribute(tag, name string) (string, bool) {
	m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Every select-all is a real submit button posting to the real endpoint. A button the script
// had to be running for — type="button", or a formaction dropped because "the script handles
// it" — is a control that does nothing at all in a browser where the file did not arrive, on
// the one page where doing nothing is not an option.
func TestTheSelectAllControlsSubmitToTheServerWithoutScript(t *testing.T) {
	rig := newConsentRig(t)
	body, requestID := rig.open(t, rig.ada)

	tags := reselectButton.FindAllStringSubmatch(body, -1)
	if len(tags) != 4 {
		t.Fatalf("want the four select-all controls, got %d: %s", len(tags), body)
	}

	seen := map[string]bool{}
	for _, tag := range tags {
		value, ok := attribute(tag[1], "value")
		if !ok {
			t.Fatalf("a select-all button carries no value: %s", tag[0])
		}
		seen[value] = true

		if action, _ := attribute(tag[1], "formaction"); action != "/authorize/reselect" {
			t.Errorf("%s posts to %q rather than to the server: %s", value, action, tag[0])
		}
		if kind, ok := attribute(tag[1], "type"); ok && kind != "submit" {
			t.Errorf("%s is type=%q, so it does nothing without script: %s", value, kind, tag[0])
		}

		// And the server understands it. Markup and handler agreeing is the property; a
		// button whose value the switch in Reselect does not name renders perfectly and
		// answers 400 when it is pressed.
		rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
			"request_id": {requestID},
			"reselect":   {value},
		})
		if rec.Code != http.StatusOK {
			t.Errorf("the server refused %q with %d: %s", value, rec.Code, rec.Body.String())
		}
	}
	for _, want := range []string{"all-mailboxes", "no-mailboxes", "all-capabilities", "no-capabilities"} {
		if !seen[want] {
			t.Errorf("no %s control on the page", want)
		}
	}
}

// The script and the server must recognise the same four selections, in the same direction.
// The dangerous drift is one way round: a value the script acts on and the server would refuse
// leaves a button that ticks boxes with script and errors without it, which is the definition
// of the two paths disagreeing.
func TestTheScriptSelectsExactlyWhatTheServerDoes(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	inPage := map[string]bool{}
	for _, tag := range reselectButton.FindAllStringSubmatch(body, -1) {
		if value, ok := attribute(tag[1], "value"); ok {
			inPage[value] = true
		}
	}

	inScript := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([a-z-]+)': \(\) =>`).FindAllStringSubmatch(string(script), -1) {
		inScript[m[1]] = true
	}
	if len(inScript) == 0 {
		t.Fatal("no selections found in the script, so this test proves nothing")
	}

	for value := range inScript {
		if !inPage[value] {
			t.Errorf("the script acts on %q, which is not a control on the page", value)
		}
	}
	for value := range inPage {
		if !inScript[value] {
			t.Logf("note: %q is a server round trip only; the script leaves it alone", value)
		}
	}
}

// The running summary of what Approve would grant is written by the script and hidden without
// it. Rendered by the server it would be right until the next tick and wrong after it, and a
// line reading "read only" above boxes that now say send is worse on this page than no line.
func TestTheDecisionSummaryIsEmptyAndHiddenWithoutScript(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	m := regexp.MustCompile(`(?s)<p([^>]*data-consent-summary[^>]*)>(.*?)</p>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no summary element on the consent screen: %s", body)
	}
	if !strings.Contains(m[1], "hidden") {
		t.Errorf("the summary is visible without script: %s", m[0])
	}
	if strings.TrimSpace(m[2]) != "" {
		t.Errorf("the server rendered summary text, which nothing keeps in step with the "+
			"boxes: %q", m[2])
	}
}

// The scope is attacker-controlled text: registration is open, and whoever registers a client
// chooses what it asks for. It is read through ParseCapability rather than printed, so what
// reaches the page is a list of names this build has, and everything else is one sentence
// rather than however many words the client sent.
func TestTheRequestedScopeIsReadRatherThanPrinted(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.openWithScope(t, rig.ada,
		"read draft solemnly-swear-this-client-is-up-to-no-good")

	for _, want := range []string{">read<", ">draft<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page should name what was recognised (%s): %s", want, body)
		}
	}
	if strings.Contains(body, "solemnly-swear") {
		t.Errorf("an unrecognised scope word reached the page: %s", body)
	}
	if !strings.Contains(body, "no capability for") {
		t.Errorf("the page should say something was asked for that it cannot grant: %s", body)
	}
	// And still nothing ticked. Marking what was asked for is not the same act as granting it.
	for _, c := range mail.AllCapabilities {
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("%s is ticked because the client asked for it", c)
		}
	}
}

// The mark beside a capability the client asked for, which is the whole of the difference the
// page draws between a request and a grant.
func TestARequestedCapabilityIsMarkedAndNotTicked(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.openWithScope(t, rig.ada, "send")

	row := regexp.MustCompile(`(?s)<label for="cap-send".*?</label>`).FindString(body)
	if row == "" {
		t.Fatalf("no send capability on the page: %s", body)
	}
	if !strings.Contains(row, ">requested<") {
		t.Errorf("send was asked for and is not marked as such: %s", row)
	}
	if !strings.Contains(row, ">privileged<") {
		t.Errorf("send is privileged and the badge is what says so in words: %s", row)
	}
	if ticked(t, body, "capabilities", "send") {
		t.Error("a requested capability must still arrive unticked")
	}

	if read := regexp.MustCompile(`(?s)<label for="cap-read".*?</label>`).FindString(body); strings.Contains(read, ">requested<") {
		t.Errorf("read was not asked for and must not be marked: %s", read)
	}
}

// The consent screen is where the split is decided, so it has to offer `discard` as its own
// box, describe both halves truthfully, and record what was ticked. A capability that exists
// in the model and never reaches this page is a capability nobody can grant.
func TestTheConsentScreenOffersDiscardSeparatelyFromDraft(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	for _, c := range []mail.Capability{mail.CapDraft, mail.CapDiscard} {
		if !hasCheckbox(body, "capabilities", string(c)) {
			t.Fatalf("the consent screen offers no %s box", c)
		}
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("%s must not be preselected", c)
		}
	}

	// The copy is the thing the operator decides on, so it is asserted rather than assumed.
	// `draft` claiming to delete drafts is exactly the sentence this change makes false.
	if strings.Contains(body, "Create, edit and delete drafts") {
		t.Error("the draft description still claims to cover deletion")
	}
	if !strings.Contains(body, "Create and edit drafts") {
		t.Errorf("the draft description does not say what draft now grants: %s", body)
	}
	if !strings.Contains(body, "Delete drafts, including ones you wrote yourself") {
		t.Errorf("the discard description is not on the page: %s", body)
	}
}

func TestApprovingRecordsDiscardWithoutDraft(t *testing.T) {
	rig := newConsentRig(t)
	ctx := context.Background()
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Approve, "/authorize/approve", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Draft tidier"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "discard"},
		"expires_days": {"90"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: want 303, got %d: %s", rec.Code, rec.Body.String())
	}

	grants, err := rig.db.ListGrants(ctx, rig.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("want exactly one grant, got %d", len(grants))
	}
	g := grants[0]
	if !g.Caps.Has(mail.CapDiscard) {
		t.Errorf("the grant did not record discard, it holds %q", g.Caps)
	}
	if g.Caps.Has(mail.CapDraft) {
		t.Errorf("the grant gained draft nobody ticked, it holds %q", g.Caps)
	}
}
