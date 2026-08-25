package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/app"
	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/mcp"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
	"github.com/tfyl/mailroom/internal/web"
)

// operatorHeader is the identity a reverse proxy would set. Forward-auth is the one operator
// login that needs no browser round trip to an issuer, which is what makes it usable from a
// test that is otherwise driving real HTTP.
const operatorHeader = "X-Forwarded-Email"

type options struct {
	// attachmentTTL is how long blob bytes live. Short values are how the tests reach the
	// expiry paths without sleeping for fifteen minutes.
	attachmentTTL time.Duration
	sendLimit     int
	sendWindow    time.Duration
	signups       signup.Policy
}

type rig struct {
	t   *testing.T
	ctx context.Context

	server  *httptest.Server
	baseURL string
	db      *store.Store
	fleet   *fleet
	blobs   *blob.Store
	holds   *held.Queue
	tools   *mcp.Tools

	// browser is the operator's. It keeps cookies, because the CSRF token is bound to one,
	// and it never follows a redirect, because the interesting ones leave this server.
	browser *http.Client
	// anon has no cookies and no identity: what a stranger holding a link sees.
	anon *http.Client

	operator string
	owner    user.User
}

func newRig(t *testing.T, opts options) *rig {
	t.Helper()
	if opts.attachmentTTL == 0 {
		opts.attachmentTTL = 15 * time.Minute
	}
	if opts.sendLimit == 0 {
		opts.sendLimit = 20
	}
	if opts.sendWindow == 0 {
		opts.sendWindow = time.Hour
	}
	if opts.signups.Mode == "" {
		opts.signups = signup.Policy{Mode: signup.Open}
	}

	dir := t.TempDir()
	ctx := t.Context()

	// The listener is bound before the handler exists, because half the server needs to know
	// its own public URL in order to be built: signed attachment links, the hold page address
	// a tool result names, and the Host allow-list on the MCP endpoint are all derived from
	// it. This is the one ordering that gets a real port into all three.
	server := httptest.NewUnstartedServer(nil)
	publicURL := "http://" + server.Listener.Addr().String()
	parsed, err := url.Parse(publicURL)
	if err != nil {
		t.Fatalf("parsing the public url: %v", err)
	}

	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generating an encryption key: %v", err)
	}
	sealer, err := secrets.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	db, err := store.Open("sqlite://" + filepath.Join(dir, "mailroom.db"))
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		PublicURL:     parsed,
		DatabaseURL:   "sqlite://" + filepath.Join(dir, "mailroom.db"),
		EncryptionKey: key,
		Signups:       opts.signups,
		Attachments: config.AttachmentConfig{
			Dir: filepath.Join(dir, "attachments"), TTL: opts.attachmentTTL,
			OwnerQuota: 32 << 20, InstanceCap: 64 << 20,
		},
	}

	blobs := blobStore(t, cfg, db)
	f := newFleet()

	providers := app.NewProviders(db, sealer, cfg)
	gate := grant.NewGate(db, db, db)
	holds := held.New(db, f, db, db, time.Hour)
	tools := mcp.NewTools(gate, f, db).
		WithBlobs(blobs).
		WithSendLimit(db, opts.sendLimit, opts.sendWindow).
		WithHoldQueue(holds, publicURL)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	oauthServer := oauthsrv.New(db, publicURL)
	mcpServer := mcp.NewServer(oauthServer, tools, publicURL, log)

	operator, err := operatorAuth()
	if err != nil {
		t.Fatalf("operator auth: %v", err)
	}
	ui, err := web.New(db, providers, sealer, operator, holds, opts.signups, publicURL, log)
	if err != nil {
		t.Fatalf("web: %v", err)
	}
	oauthServer.ConsentPage = ui.ConsentPage

	mux := http.NewServeMux()
	oauthServer.Routes(mux)
	mcpServer.Routes(mux)
	blob.NewServer(blobs, db, db, log).Routes(mux)
	ui.Routes(mux, oauthServer)

	server.Config.Handler = web.SecurityHeaders(providers.AuthOrigins(), mux)
	server.Start()
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	r := &rig{
		t: t, ctx: ctx, server: server, baseURL: publicURL, db: db, fleet: f,
		blobs: blobs, holds: holds, tools: tools,
		browser:  &http.Client{Jar: jar, CheckRedirect: noRedirect},
		anon:     &http.Client{CheckRedirect: noRedirect},
		operator: "ada@example.com",
	}

	// One authenticated GET creates the operator's user row, the way a first sign-in does.
	r.get("/accounts")
	users, err := db.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("expected exactly one user after the first sign-in, got %d (%v)", len(users), err)
	}
	r.owner = users[0]
	return r
}

func blobStore(t *testing.T, cfg *config.Config, db *store.Store) *blob.Store {
	t.Helper()
	key, err := secrets.Derive(cfg.EncryptionKey, blob.SigningPurpose, 32)
	if err != nil {
		t.Fatalf("deriving the signing key: %v", err)
	}
	signer, err := blob.NewSigner(key)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	dir, err := blob.NewDir(cfg.Attachments.Dir)
	if err != nil {
		t.Fatalf("attachment dir: %v", err)
	}
	return blob.New(dir, db, signer, cfg.PublicURL.String(), blob.Options{
		TTL:         cfg.Attachments.TTL,
		OwnerQuota:  cfg.Attachments.OwnerQuota,
		InstanceCap: cfg.Attachments.InstanceCap,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func operatorAuth() (*auth.Registry, error) {
	registry := auth.NewRegistry(auth.NewSessions(time.Hour))
	forward, err := auth.NewForward(operatorHeader, []string{"127.0.0.1/32", "::1/128"}, "")
	if err != nil {
		return nil, err
	}
	registry.SetForward(forward)
	return registry, registry.Validate()
}

// --- the operator's browser ---

func (r *rig) get(path string) *http.Response {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		r.t.Fatalf("building GET %s: %v", path, err)
	}
	req.Header.Set(operatorHeader, r.operator)
	resp, err := r.browser.Do(req)
	if err != nil {
		r.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// page fetches a page and returns its body alongside the CSRF token it rendered.
func (r *rig) page(path string) (string, string) {
	r.t.Helper()
	resp := r.get(path)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("reading %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("GET %s: %d\n%s", path, resp.StatusCode, body)
	}
	return string(body), csrfFrom(string(body))
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

func csrfFrom(body string) string {
	m := csrfPattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func (r *rig) post(path string, form url.Values) *http.Response {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.baseURL+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		r.t.Fatalf("building POST %s: %v", path, err)
	}
	req.Header.Set(operatorHeader, r.operator)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.browser.Do(req)
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// --- linking a mailbox ---
//
// Linking normally happens through a provider's OAuth callback, which needs the provider.
// The row it writes is the whole of what the rest of the server sees, so the tests write that
// row directly and leave the flow that produces it to internal/web's own tests.

func (r *rig) link(alias, address string) mail.Account {
	r.t.Helper()
	acct := mail.Account{
		ID: mail.AccountID(ids.New("acct")), OwnerID: r.owner.ID,
		Alias: alias, Address: address,
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	if err := r.db.LinkAccount(r.ctx, r.owner.ID, acct, "not-a-real-credential", ""); err != nil {
		r.t.Fatalf("linking %s: %v", alias, err)
	}
	return acct
}

func (r *rig) mailbox(acct mail.Account) *mailbox { return r.fleet.box(acct) }

// --- the client's OAuth flow ---

type client struct {
	id       string
	redirect string
}

// register performs RFC 7591 dynamic registration, as a client with no prior relationship to
// this server does.
func (r *rig) register(name string) client {
	r.t.Helper()
	redirect := "http://127.0.0.1:1/callback"
	body, _ := json.Marshal(map[string]any{
		"client_name": name, "redirect_uris": []string{redirect},
	})
	resp, err := r.anon.Post(r.baseURL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		r.t.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		r.t.Fatalf("POST /register: %d\n%s", resp.StatusCode, raw)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		r.t.Fatalf("decoding the registration: %v", err)
	}
	if out.ClientID == "" {
		r.t.Fatal("registration returned no client_id")
	}
	return client{id: out.ClientID, redirect: redirect}
}

type approval struct {
	label    string
	accounts []mail.Account
	caps     []mail.Capability
	mode     grant.Mode
	expires  string
}

// authorize walks the whole authorization flow and answers with the bearer token.
//
// Every step is a real request: the consent screen is fetched and its request id and CSRF
// token are read out of the rendered HTML, the approval is posted as the form posts it, the
// code is taken from the Location header, and it is exchanged at /token with the PKCE
// verifier.
func (r *rig) authorize(c client, a approval) (string, grant.ID) {
	r.t.Helper()

	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("client_id", c.id)
	q.Set("redirect_uri", c.redirect)
	q.Set("response_type", "code")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "the-state")
	for _, cap := range a.caps {
		q.Add("scope", string(cap))
	}

	body, csrf := r.page("/authorize?" + q.Encode())
	if csrf == "" {
		r.t.Fatal("the consent screen rendered no CSRF token")
	}
	requestID := attrValue(body, "request_id")
	if requestID == "" {
		r.t.Fatalf("the consent screen rendered no request_id:\n%s", body)
	}

	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("request_id", requestID)
	form.Set("label", a.label)
	form.Set("expires_days", cmpOr(a.expires, "never"))
	if a.mode != "" {
		form.Set("mode", string(a.mode))
	}
	for _, acct := range a.accounts {
		form.Add("accounts", string(acct.ID))
	}
	for _, cap := range a.caps {
		form.Add("capabilities", string(cap))
	}

	resp := r.post("/authorize/approve", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		r.t.Fatalf("POST /authorize/approve: %d\n%s", resp.StatusCode, raw)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		r.t.Fatalf("parsing the approval redirect: %v", err)
	}
	code := location.Query().Get("code")
	if code == "" {
		r.t.Fatalf("the approval redirected without a code: %s", location)
	}
	if got := location.Query().Get("state"); got != "the-state" {
		r.t.Fatalf("state came back as %q", got)
	}

	token := url.Values{}
	token.Set("grant_type", "authorization_code")
	token.Set("code", code)
	token.Set("client_id", c.id)
	token.Set("redirect_uri", c.redirect)
	token.Set("code_verifier", verifier)
	tokenResp, err := r.anon.PostForm(r.baseURL+"/token", token)
	if err != nil {
		r.t.Fatalf("POST /token: %v", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tokenResp.Body)
		r.t.Fatalf("POST /token: %d\n%s", tokenResp.StatusCode, raw)
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&issued); err != nil {
		r.t.Fatalf("decoding the token response: %v", err)
	}
	if issued.AccessToken == "" || issued.TokenType != "Bearer" {
		r.t.Fatalf("token response was %+v", issued)
	}

	// The grant id is not in the token response — deliberately, since a client has no use for
	// it — so it is read back the way the operator's page does.
	grants, err := r.db.ListGrants(r.ctx, r.owner.ID)
	if err != nil || len(grants) == 0 {
		r.t.Fatalf("listing grants: %v", err)
	}
	var id grant.ID
	for _, g := range grants {
		if g.Label == a.label {
			id = g.ID
		}
	}
	if id == "" {
		r.t.Fatalf("no grant labelled %q was recorded", a.label)
	}
	return issued.AccessToken, id
}

// grantFor is the whole flow in one call, for a test whose subject is what happens after it.
func (r *rig) grantFor(a approval) (*session, grant.ID) {
	r.t.Helper()
	s, _, id := r.grantWithToken(a)
	return s, id
}

// grantWithToken is grantFor for a test that needs the bearer token itself, because what it
// is asking is whether a token still works.
func (r *rig) grantWithToken(a approval) (*session, string, grant.ID) {
	r.t.Helper()
	c := r.register(a.label + " client")
	token, id := r.authorize(c, a)
	return r.connect(token), token, id
}

// consentPage renders the consent screen for a registered client, without approving it. It is
// how a test reads what an operator is actually shown.
func (r *rig) consentPage(c client) string {
	r.t.Helper()
	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{}
	q.Set("client_id", c.id)
	q.Set("redirect_uri", c.redirect)
	q.Set("response_type", "code")
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	body, _ := r.page("/authorize?" + q.Encode())
	return body
}

// --- MCP over the real transport ---

type session struct {
	t   *testing.T
	ctx context.Context
	cs  *sdk.ClientSession
}

// connect performs MCP initialize over Streamable HTTP with the bearer token, exactly as a
// client would.
func (r *rig) connect(token string) *session {
	r.t.Helper()
	transport := &sdk.StreamableClientTransport{
		Endpoint: r.baseURL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{
			token: token, base: http.DefaultTransport,
		}},
		// The server is stateless, so there is no server-initiated traffic to wait for.
		DisableStandaloneSSE: true,
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "test"}, nil)
	cs, err := c.Connect(r.ctx, transport, nil)
	if err != nil {
		r.t.Fatalf("MCP initialize: %v", err)
	}
	r.t.Cleanup(func() { _ = cs.Close() })
	return &session{t: r.t, ctx: r.ctx, cs: cs}
}

// connectExpectingFailure is initialize with a token this server should refuse.
func (r *rig) connectExpectingFailure(token string) error {
	transport := &sdk.StreamableClientTransport{
		Endpoint:             r.baseURL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "test"}, nil)
	cs, err := c.Connect(r.ctx, transport, nil)
	if err == nil {
		_ = cs.Close()
		return nil
	}
	return err
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

func (s *session) toolNames() []string {
	s.t.Helper()
	res, err := s.cs.ListTools(s.ctx, &sdk.ListToolsParams{})
	if err != nil {
		s.t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names
}

func (s *session) tools() map[string]*sdk.Tool {
	s.t.Helper()
	res, err := s.cs.ListTools(s.ctx, &sdk.ListToolsParams{})
	if err != nil {
		s.t.Fatalf("tools/list: %v", err)
	}
	out := map[string]*sdk.Tool{}
	for _, t := range res.Tools {
		out[t.Name] = t
	}
	return out
}

// toolResult is a tool call's answer, unpacked far enough to assert on.
type toolResult struct {
	isError bool
	text    string
	payload map[string]any
}

func (r toolResult) String() string { return r.text }

func (s *session) call(name string, args map[string]any) toolResult {
	s.t.Helper()
	res, err := s.cs.CallTool(s.ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("tools/call %s: %v", name, err)
	}
	out := toolResult{isError: res.IsError}
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			out.text = text.Text
		}
	}
	_ = json.Unmarshal([]byte(out.text), &out.payload)
	return out
}

// callError is a call this test expects to be refused: it fails if it was not.
func (s *session) callError(name string, args map[string]any) toolResult {
	s.t.Helper()
	res := s.call(name, args)
	if !res.isError {
		s.t.Fatalf("%s was expected to be refused and answered:\n%s", name, res.text)
	}
	return res
}

// callOK is a call this test expects to work.
func (s *session) callOK(name string, args map[string]any) toolResult {
	s.t.Helper()
	res := s.call(name, args)
	if res.isError {
		s.t.Fatalf("%s failed:\n%s", name, res.text)
	}
	return res
}

// --- the audit log, as the page reads it ---

func (r *rig) audit() []store.AuditEntry {
	r.t.Helper()
	rows, err := r.db.RecentAudit(r.ctx, r.owner.ID, 500)
	if err != nil {
		r.t.Fatalf("reading the audit log: %v", err)
	}
	return rows
}

// auditFor returns the rows one tool wrote, newest first.
func (r *rig) auditFor(tool string) []store.AuditEntry {
	r.t.Helper()
	var out []store.AuditEntry
	for _, row := range r.audit() {
		if row.Tool == tool {
			out = append(out, row)
		}
	}
	return out
}

func (r *rig) lastAuditFor(tool string) store.AuditEntry {
	r.t.Helper()
	rows := r.auditFor(tool)
	if len(rows) == 0 {
		r.t.Fatalf("no audit row for %s; the log holds:\n%s", tool, r.auditDump())
	}
	return rows[0]
}

func (r *rig) auditDump() string {
	var b strings.Builder
	for _, row := range r.audit() {
		affected := "-"
		if row.Affected != nil {
			affected = fmt.Sprint(*row.Affected)
		}
		fmt.Fprintf(&b, "  %-24s outcome=%-14s cap=%-12s affected=%-4s grant=%q reason=%q detail=%+v\n",
			row.Tool, row.Outcome, row.Capability, affected, row.GrantName, row.Reason, row.Detail)
	}
	return b.String()
}

// --- the held queue, as the page shows it ---

func (r *rig) pending() []held.Action {
	r.t.Helper()
	actions, err := r.holds.Pending(r.ctx, r.owner.ID)
	if err != nil {
		r.t.Fatalf("reading the held queue: %v", err)
	}
	return actions
}

// approveHeld presses Approve on the page, CSRF token and all.
func (r *rig) approveHeld(id string) *http.Response {
	r.t.Helper()
	form := url.Values{"csrf_token": {r.csrfToken()}, "id": {id}}
	return r.post("/held/approve", form)
}

func (r *rig) declineHeld(id string) *http.Response {
	r.t.Helper()
	form := url.Values{"csrf_token": {r.csrfToken()}, "id": {id}}
	return r.post("/held/decline", form)
}

// csrfToken reads a token off whichever page is rendering one.
//
// It is bound to the browser's cookie rather than to a page, so any of these will do — and
// taking the first that answers matters, because the pages that are interesting to post to
// stop rendering forms once their list is empty. A queue with nothing waiting draws no
// buttons, so a token scraped from it would be the empty string and every POST after that
// would be refused for the wrong reason.
func (r *rig) csrfToken() string {
	r.t.Helper()
	for _, path := range []string{"/invites", "/accounts", "/grants", "/held"} {
		resp := r.get(path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// A page this operator may not see is skipped rather than fatal: /invites is the
		// instance owner's, and half of these tests are a second operator.
		if resp.StatusCode != http.StatusOK {
			continue
		}
		if token := csrfFrom(string(body)); token != "" {
			return token
		}
	}
	r.t.Fatal("no page in this UI rendered a CSRF token")
	return ""
}

// --- operator actions on a grant ---

// revoke presses Revoke and then confirms, which is what the page makes an operator do: the
// first POST renders a confirmation, and only a second one carrying confirm=yes revokes.
func (r *rig) revoke(id grant.ID) {
	r.t.Helper()
	resp := r.post("/grants/revoke",
		url.Values{"csrf_token": {r.csrfToken()}, "id": {string(id)}, "confirm": {"yes"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		r.t.Fatalf("revoking %s: %d\n%s", id, resp.StatusCode, body)
	}
	g, err := r.db.Grant(r.ctx, id)
	if err != nil || !g.Revoked() {
		r.t.Fatalf("the grant did not come back revoked: %v %v", g, err)
	}
}

func (r *rig) removeGrant(id grant.ID) {
	r.t.Helper()
	resp := r.post("/grants/remove", url.Values{"csrf_token": {r.csrfToken()}, "id": {string(id)}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		r.t.Fatalf("removing %s: %d\n%s", id, resp.StatusCode, body)
	}
	if _, err := r.db.Grant(r.ctx, id); err == nil {
		r.t.Fatalf("the grant is still loadable after being removed")
	}
}

// --- fetching a signed URL, as the holder of a link does ---

func (r *rig) fetch(link string) (int, []byte, http.Header) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, link, nil)
	if err != nil {
		r.t.Fatalf("building the fetch: %v", err)
	}
	resp, err := r.anon.Do(req)
	if err != nil {
		r.t.Fatalf("fetching %s: %v", link, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header
}

func (r *rig) put(link string, body []byte) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPut, link, strings.NewReader(string(body)))
	if err != nil {
		r.t.Fatalf("building the upload: %v", err)
	}
	resp, err := r.anon.Do(req)
	if err != nil {
		r.t.Fatalf("uploading to %s: %v", link, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// --- small helpers ---

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func cmpOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// attrValue pulls one hidden input's value out of rendered HTML.
func attrValue(body, name string) string {
	re := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

var _ = user.User{}
