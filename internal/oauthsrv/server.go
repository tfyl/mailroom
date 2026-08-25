// Package oauthsrv is the OAuth 2.1 authorization server MCP clients authenticate against.
//
// This is the inbound plane. It never touches provider credentials; it decides what an MCP
// client may ask mailroom to do with them, which is a different question and deliberately a
// different code path.
package oauthsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

type Server struct {
	store     *store.Store
	publicURL string
	// Pending consent screens and issued authorization codes are held apart. One namespace
	// made a request id a second, fully valid authorization code: both are opaque keys into
	// the same map, and nothing distinguished a key the operator had merely been shown from
	// one they had approved.
	pending  *codeStore
	codes    *codeStore
	tokenTTL time.Duration
	now      func() time.Time

	// ConsentPage renders the approval form. Supplied by the web layer so this package owns
	// protocol rather than presentation.
	ConsentPage func(w http.ResponseWriter, r *http.Request, req ConsentRequest)
}

// clientName trims what an unauthenticated registration puts on the consent screen.
//
// Registration is open by design — that is what dynamic client registration is — so the name
// is attacker-controlled text on the one page whose job is helping a human decide. Escaping
// stops it becoming markup; length is what stops it becoming a paragraph of reassuring prose
// above the buttons. Newlines go for the same reason: a name is one line.
func clientName(raw string) string {
	name := strings.Join(strings.Fields(raw), " ")
	if len(name) > maxClientNameLen {
		name = name[:maxClientNameLen] + "…"
	}
	return firstNonEmpty(name, "Unnamed client")
}

// maxClientNameLen is long enough for any honest product name and short of a sentence.
const maxClientNameLen = 64

// ConsentRequest is everything the consent form needs to render.
type ConsentRequest struct {
	RequestID     string
	ClientName    string
	ClientID      string
	Accounts      []mail.Account
	Capabilities  []mail.Capability
	RequestedCaps []string // shown as a suggestion; never preselected

	// Redirect is where an approved authorization code is actually sent. The name above the
	// buttons is a claim an unauthenticated registration made about itself; this is the only
	// thing on the screen the client had to commit to in advance and cannot restate.
	Redirect RedirectTarget

	// What the operator has already chosen, carried back when the screen is re-rendered by
	// Reselect. All four are empty on a first render, which is what keeps a fresh consent
	// screen from arriving with anything pre-granted.
	SelectedAccounts []mail.AccountID
	SelectedCaps     []mail.Capability
	Label            string
	ExpiresDays      string
	// Mode is how much initiative the client is being given. Unlike the capabilities it has
	// a default on a first render: every grant has a mode whether or not anybody chose one,
	// so an unticked radiogroup would be a lie about what the grant would do. The default is
	// the middle setting, and the screen says which one it is and why.
	Mode grant.Mode
}

// RedirectTarget is the callback destination as the consent screen has to state it.
//
// The origin and nothing more. A whole URI on that page would be a string a registration
// wrote most of: a long enough path or query pushes the host out of sight on a narrow screen
// or behind an ellipsis, and the host is the only part of it that identifies anybody. Cutting
// at the origin means what the operator reads cannot be padded.
type RedirectTarget struct {
	// Origin is the scheme and host. Empty when the redirect could not be read at all, in
	// which case the screen says nothing rather than guessing at a destination.
	Origin string
	// Kind is which of the three destinations this server accepts this one is. They are three
	// different questions for an operator rather than three spellings of one, so the screen
	// asks them in different words.
	Kind string
	// ASCIIHost is false when the host is not written in ASCII. A name drawn from other
	// alphabets can be made to read as a name from this one, which is the failure the whole
	// idea of showing a host has to survive. Registration refuses such a host now, so this
	// covers a client that registered before it did.
	ASCIIHost bool
}

const (
	// RedirectRemote is a host out on the internet, and the case where the operator has to
	// recognise something.
	RedirectRemote = "remote"
	// RedirectLoopback is a program listening on this machine. Nobody can collect a code
	// there without already being on the machine, so it is a materially weaker claim on the
	// operator's attention than a remote host — and a screen that shouted at every callback
	// equally would be a screen that shouted at the ordinary desktop client every time.
	RedirectLoopback = "loopback"
	// RedirectScheme is a private-use scheme such as cursor://. The operating system hands
	// the callback to whichever installed program claims the scheme, so it is local without
	// being anything the operator can check.
	RedirectScheme = "scheme"
)

// describeRedirect reduces a registered redirect URI to what the consent screen shows.
//
// The origin is formActionSource's, deliberately rather than incidentally: the string the
// operator reads and the string this response's own form-action policy names are then one
// string computed once. Parsing the URI a second time here would be a second answer that
// could differ from the first, and a consent screen naming one destination while its policy
// permits another is exactly the confusion it exists to remove.
func describeRedirect(raw string) RedirectTarget {
	origin := formActionSource(raw)
	if origin == "" {
		return RedirectTarget{}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return RedirectTarget{}
	}
	// Hostname(), not Host: it drops the port and, more to the point, the userinfo, so a
	// redirect registered as https://claude.ai@evil.example is described by the host that
	// will actually receive the code. formActionSource drops it from the origin for the same
	// reason, one layer down.
	host := u.Hostname()
	t := RedirectTarget{Origin: origin, ASCIIHost: asciiOnly(host)}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		t.Kind = RedirectScheme
	case loopbackHost(host):
		t.Kind = RedirectLoopback
	default:
		t.Kind = RedirectRemote
	}
	return t
}

// loopbackHost is this machine, and nothing that merely reads like it. An address is decided
// by net.ParseIP rather than by a prefix, so 127.0.0.1.evil.example is a name; "localhost" is
// matched whole, so localhost.evil.example is a name too. Both are remote hosts and the
// screen says so.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func New(st *store.Store, publicURL string) *Server {
	return &Server{
		store:     st,
		publicURL: strings.TrimSuffix(publicURL, "/"),
		pending:   newCodeStore(10 * time.Minute),
		codes:     newCodeStore(10 * time.Minute),
		tokenTTL:  0, // access tokens do not expire on their own; a grant's expiry governs
		now:       time.Now,
	}
}

// Routes registers the endpoints that must be reachable without an interactive login. A
// remote MCP client has no browser attached, so anything a proxy protects with an
// interactive challenge is unreachable to it — see docs/deploying.md.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("POST /register", s.register)
	mux.HandleFunc("POST /token", s.token)
}

func (s *Server) metadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.publicURL,
		"authorization_endpoint":                s.publicURL + "/authorize",
		"token_endpoint":                        s.publicURL + "/token",
		"registration_endpoint":                 s.publicURL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      capabilityStrings(),
	})
}

func (s *Server) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.publicURL + "/mcp",
		"authorization_servers": []string{s.publicURL},
		"scopes_supported":      capabilityStrings(),
	})
}

// register implements dynamic client registration. Registration is open because an MCP
// client must be able to introduce itself before a human has ever seen it — but registering
// grants nothing at all. Every capability still comes from a consent screen the operator
// approves.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read registration body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}

	c := store.Client{
		ID:           ids.Client(),
		Name:         clientName(req.ClientName),
		RedirectURIs: req.RedirectURIs,
	}
	if err := s.store.RegisterClient(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
	})
}

// Authorize handles GET /authorize. It runs behind operator authentication: the consent
// screen is the one place a human decides what a client may do, so it must be a human who
// sees it.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")

	client, err := s.store.Client(r.Context(), clientID)
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}

	redirect := q.Get("redirect_uri")
	if !allowedRedirect(client.RedirectURIs, redirect) {
		// Never redirect to an unregistered URI, even to report the error — that would turn
		// the authorization endpoint into an open redirector.
		http.Error(w, "redirect_uri does not match this client's registration", http.StatusBadRequest)
		return
	}
	// From here on the redirect is one the client registered, so the consent screen may name
	// it in its own form-action. Doing it here rather than at the render covers the error
	// redirects below as well, and nothing has written a byte yet.
	allowRedirectInFormAction(w, redirect)

	if q.Get("response_type") != "code" {
		redirectError(w, r, redirect, q.Get("state"), "unsupported_response_type", "only response_type=code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		redirectError(w, r, redirect, q.Get("state"), "invalid_request", "code_challenge is required")
		return
	}
	method := q.Get("code_challenge_method")
	if method != "S256" {
		redirectError(w, r, redirect, q.Get("state"), "invalid_request", "code_challenge_method must be S256")
		return
	}

	// The consent screen offers only the signed-in user's mailboxes. On a shared instance
	// this is the difference between approving access to your own mail and being shown a
	// list of everybody's.
	operator, ok := user.FromContext(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context(), operator.ID)
	if err != nil {
		http.Error(w, "could not load accounts", http.StatusInternalServerError)
		return
	}

	requestID := ids.New("req")
	s.pending.put(requestID, &pendingAuth{
		Owner:         operator.ID,
		ClientID:      clientID,
		RedirectURI:   redirect,
		State:         q.Get("state"),
		Challenge:     challenge,
		ChallengeAlgo: method,
		RequestedCaps: strings.Fields(q.Get("scope")),
	})

	s.ConsentPage(w, r, ConsentRequest{
		RequestID:     requestID,
		ClientName:    client.Name,
		ClientID:      clientID,
		Accounts:      accounts,
		Capabilities:  mail.AllCapabilities,
		RequestedCaps: strings.Fields(q.Get("scope")),
		// The redirect has already been checked against this client's registration above, so
		// what the screen names is a URI the client committed to before anybody was looking
		// at it, not one the query string just asked for.
		Redirect: describeRedirect(redirect),
	})
}

// Approve handles the consent form submission and issues an authorization code.
func (s *Server) Approve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	// Consumed, not read. Leaving it in place made the consent form replayable: a second
	// submission minted a second grant, and because the entry was shared by pointer it
	// repointed every code already issued at the newest one — so a code obtained under a
	// read-only consent redeemed for send and destructive. It also meant Deny after Approve
	// told the client it was refused while leaving the grant and the code alive.
	pending, ok := s.pending.take(r.FormValue("request_id"))
	if !ok {
		http.Error(w, "this authorization request expired; start again from your client", http.StatusBadRequest)
		return
	}

	// Approve as the user who is signed in now, and refuse if that is not the user the
	// screen was rendered for. Otherwise a request id leaked or replayed into another
	// session would let one user approve access to another's mailboxes.
	operator, ok := user.FromContext(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	if pending.Owner != operator.ID {
		http.Error(w, "this authorization request belongs to a different session", http.StatusForbidden)
		return
	}

	accounts := r.Form["accounts"]
	if len(accounts) == 0 {
		http.Error(w, "select at least one mailbox", http.StatusBadRequest)
		return
	}
	caps, err := mail.SetFromStrings(r.Form["capabilities"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if caps.Len() == 0 {
		http.Error(w, "select at least one capability", http.StatusBadRequest)
		return
	}

	accountIDs := make([]mail.AccountID, 0, len(accounts))
	for _, a := range accounts {
		accountIDs = append(accountIDs, mail.AccountID(a))
	}

	// The mode is read the same way the edit page reads it, so a value this screen would
	// refuse cannot be spelled into a grant through that form instead. An empty field is the
	// default rather than an error: a client posting straight at this endpoint without one
	// gets the middle setting, never the loose one.
	mode := grant.DefaultMode
	if chosen := r.FormValue("mode"); chosen != "" {
		mode, err = grant.ParseMode(chosen)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	g := &grant.Grant{
		ID:       grant.ID(ids.Grant()),
		OwnerID:  operator.ID,
		ClientID: pending.ClientID,
		Label:    firstNonEmpty(strings.TrimSpace(r.FormValue("label")), "Unnamed grant"),
		Accounts: accountIDs,
		Caps:     caps,
		Mode:     mode,
	}
	// Expiry is a security control, so anything unreadable is refused rather than quietly
	// becoming a grant that never expires. The reading is grant.ParseExpiry so that the edit
	// page, which sets the same field on a grant that already exists, cannot accept a value
	// this screen would have refused.
	expires, err := grant.ParseExpiry(r.FormValue("expires_days"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.ExpiresAt = expires
	if err := s.store.CreateGrant(r.Context(), g); err != nil {
		http.Error(w, "could not record the grant", http.StatusInternalServerError)
		return
	}

	code, err := ids.Token()
	if err != nil {
		http.Error(w, "could not issue a code", http.StatusInternalServerError)
		return
	}
	issued := *pending
	issued.GrantID = string(g.ID)
	s.codes.put(code, &issued)

	target, _ := url.Parse(pending.RedirectURI)
	q := target.Query()
	q.Set("code", code)
	if pending.State != "" {
		q.Set("state", pending.State)
	}
	target.RawQuery = q.Encode()
	allowRedirectInFormAction(w, pending.RedirectURI)
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// Reselect re-renders the consent screen with one group of boxes ticked, or cleared, and
// every other choice carried back untouched.
//
// This is a server round trip because the operator interface ships no JavaScript, so there is
// no client-side toggle to be had. A CSS-only trick is worse than none: it can change how a
// checkbox looks without changing what the form submits, and on a consent screen a control
// that appears to grant one thing while submitting another is the failure that matters.
//
// It is a separate endpoint rather than a branch inside Approve because routing, not a
// conditional, is then what keeps the two apart. A misread button value here re-renders the
// wrong group, which the operator sees and can correct before approving; a misread branch in
// Approve would issue a grant.
func (s *Server) Reselect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	operator, ok := user.FromContext(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}

	// Read, never take. The operator has not decided anything yet, and the page this renders
	// submits the same request id again to approve or deny — consuming it here would answer
	// the next submission with "this authorization request expired".
	requestID := r.FormValue("request_id")
	pending, ok := s.pending.get(requestID)
	if !ok {
		http.Error(w, "this authorization request expired; start again from your client", http.StatusBadRequest)
		return
	}
	if pending.Owner != operator.ID {
		http.Error(w, "this authorization request belongs to a different session", http.StatusForbidden)
		return
	}

	client, err := s.store.Client(r.Context(), pending.ClientID)
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	// Re-read rather than trusting the form. The mailbox list is the signed-in operator's
	// own, so it is the same list select-all can tick and the same list a submitted id has to
	// appear in to survive the round trip.
	accounts, err := s.store.ListAccounts(r.Context(), operator.ID)
	if err != nil {
		http.Error(w, "could not load accounts", http.StatusInternalServerError)
		return
	}

	// Anything submitted that is not on offer is dropped rather than refused. The boxes are
	// rebuilt from the operator's own mailboxes and from the known capabilities, so a value
	// outside those has no box to tick, and carrying it forward is the only thing that could
	// do harm. Refusing instead would turn an ordinary race — a mailbox unlinked in another
	// tab while this consent screen sat open — into a dead end on a page that is otherwise
	// still perfectly recoverable. Nothing is granted either way: Approve is the gate, and it
	// refuses a mailbox that is not the operator's.
	selectedAccounts := ownedAccounts(accounts, r.Form["accounts"])
	selectedCaps := knownCapabilities(r.Form["capabilities"])

	switch r.FormValue("reselect") {
	case "all-mailboxes":
		selectedAccounts = allAccountIDs(accounts)
	case "no-mailboxes":
		selectedAccounts = nil
	case "all-capabilities":
		selectedCaps = append([]mail.Capability(nil), mail.AllCapabilities...)
	case "no-capabilities":
		selectedCaps = nil
	default:
		http.Error(w, "unknown selection", http.StatusBadRequest)
		return
	}

	s.ConsentPage(w, r, ConsentRequest{
		RequestID:     requestID,
		ClientName:    client.Name,
		ClientID:      pending.ClientID,
		Accounts:      accounts,
		Capabilities:  mail.AllCapabilities,
		RequestedCaps: pending.RequestedCaps,
		// Read from the pending request rather than from the form, like the owner and the
		// scope. A select-all must not be a way to redraw the screen with a destination the
		// submission chose: the redirect is fixed when the request is created and this is the
		// same one Approve will send the code to.
		Redirect:         describeRedirect(pending.RedirectURI),
		SelectedAccounts: selectedAccounts,
		SelectedCaps:     selectedCaps,
		Label:            strings.TrimSpace(r.FormValue("label")),
		ExpiresDays:      r.FormValue("expires_days"),
		// Carried back untouched, like every other choice on the screen. Anything
		// unrecognised falls back to the default rather than being refused, for the same
		// reason an unknown mailbox id is dropped here: this endpoint grants nothing, and a
		// recoverable page beats a dead end. Approve is the gate, and it refuses there.
		Mode: grant.Mode(r.FormValue("mode")).Resolved(),
	})
}

// ownedAccounts keeps only the submitted ids that belong to the signed-in operator, in the
// order the mailboxes are displayed in.
func ownedAccounts(owned []mail.Account, submitted []string) []mail.AccountID {
	want := make(map[string]bool, len(submitted))
	for _, id := range submitted {
		want[id] = true
	}
	out := make([]mail.AccountID, 0, len(submitted))
	for _, a := range owned {
		if want[string(a.ID)] {
			out = append(out, a.ID)
		}
	}
	return out
}

func allAccountIDs(accounts []mail.Account) []mail.AccountID {
	out := make([]mail.AccountID, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.ID)
	}
	return out
}

// knownCapabilities keeps only the submitted names that are real capabilities, in the
// canonical display order.
func knownCapabilities(submitted []string) []mail.Capability {
	set := mail.NewSet()
	for _, name := range submitted {
		if c, err := mail.ParseCapability(name); err == nil {
			set.Add(c)
		}
	}
	return set.Slice()
}

// Deny lets the operator refuse without issuing anything.
func (s *Server) Deny(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	operator, ok := user.FromContext(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}

	// Read before consuming, so somebody else's pending request is not cancelled on the way
	// to discovering it was not theirs. Approve checks the same thing; Deny not checking it
	// made cancelling another user's authorization free to anyone who learned a request id.
	pending, ok := s.pending.get(r.FormValue("request_id"))
	if !ok {
		http.Error(w, "this authorization request expired", http.StatusBadRequest)
		return
	}
	if pending.Owner != operator.ID {
		http.Error(w, "this authorization request belongs to a different session", http.StatusForbidden)
		return
	}
	s.pending.take(r.FormValue("request_id"))
	allowRedirectInFormAction(w, pending.RedirectURI)
	redirectError(w, r, pending.RedirectURI, pending.State, "access_denied", "the operator declined this request")
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read form")
		return
	}
	if r.FormValue("grant_type") != "authorization_code" {
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}

	// Single-use: taking the code removes it, so a replayed code cannot mint a second token.
	pending, ok := s.codes.take(r.FormValue("code"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid, expired, or already used")
		return
	}
	if pending.GrantID == "" {
		writeError(w, http.StatusBadRequest, "invalid_grant", "authorization code was never approved")
		return
	}
	if pending.ClientID != r.FormValue("client_id") {
		writeError(w, http.StatusBadRequest, "invalid_grant", "this code was issued to a different client")
		return
	}
	if err := verifyPKCE(pending.Challenge, pending.ChallengeAlgo, r.FormValue("code_verifier")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	// RFC 6749 requires the redirect_uri to match the one the code was issued against. PKCE
	// and exact-match registration already bind the code, so this is defence in depth rather
	// than the only thing standing there — but a client that sends a different one is a
	// client something has gone wrong with.
	if got := r.FormValue("redirect_uri"); got != "" && got != pending.RedirectURI {
		writeError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the one this code was issued for")
		return
	}

	g, err := s.store.Grant(r.Context(), grant.ID(pending.GrantID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", "the grant no longer exists")
		return
	}
	// Revoking between approval and redemption is a narrow window, but the operator revoked
	// it — issuing a token afterwards would mean the grants page said one thing and the
	// server did another. The MCP boundary refuses the token anyway; refusing to mint it is
	// where the operator's decision should take effect. Asking the grant itself rather than
	// re-deriving the rule keeps one definition of what makes a grant usable.
	if err := g.Valid(time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	token, err := ids.Token()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
		return
	}
	if err := s.store.IssueToken(r.Context(), token, g.ID, g.ExpiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not record the token")
		return
	}

	body := map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"scope":        g.Caps.String(),
	}
	if g.ExpiresAt != nil {
		body["expires_in"] = int(time.Until(*g.ExpiresAt).Seconds())
	}
	writeJSON(w, http.StatusOK, body)
}

// GrantForRequest resolves the bearer token on an MCP request to its grant. The grant is
// re-read every call, so revoking one takes effect immediately rather than at token expiry.
func (s *Server) GrantForRequest(ctx context.Context, r *http.Request) (*grant.Grant, error) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return nil, grant.ErrNotFound
	}
	g, err := s.store.GrantForToken(ctx, strings.TrimSpace(token))
	if err != nil {
		return nil, err
	}
	s.markUsed(ctx, g)
	return g, nil
}

// markUsed records that a client presented this grant.
//
// Used means presented, not authorised. This runs before the grant is checked for revocation
// or expiry and before any capability is tested, because a token being offered at all is what
// the operator is reading the page to find out: a client still calling with a revoked grant,
// or one being refused every capability it asks for, is the case where a stale "never used"
// is most misleading. Hanging this off the capability check instead would have shown the
// busiest grant as dormant exactly when something had started going wrong with it.
//
// The write is coarsened to grant.TouchInterval. Every MCP request comes through here and
// clients poll, so writing each time would put a SQLite write on the hot path to keep a value
// the page renders to the minute.
//
// A failure is logged and dropped. Last use is a display; failing a mail call because a
// timestamp could not be stored would trade a cosmetic problem for a real one.
func (s *Server) markUsed(ctx context.Context, g *grant.Grant) {
	now := s.now()
	if !g.NeedsTouch(now) {
		return
	}
	if err := s.store.TouchGrant(ctx, g.OwnerID, g.ID, now); err != nil {
		slog.Default().Warn("could not record a grant's last use", "grant", g.ID, "err", err)
		return
	}
	// The caller is handed the grant it just used, so the timestamp it carries should be the
	// one now in the database rather than the one read a moment before the write.
	g.LastUsedAt = &now
}

// allowRedirectInFormAction widens this one response's form-action directive to admit the
// redirect the operator is being asked about.
//
// The base policy is fixed for the whole server and lists only 'self' and the identity
// providers, but `form-action` governs the entire redirect chain a form submission sets off,
// not just its immediate target — and the consent form's chain ends at the MCP client's
// callback. Without this the browser refuses that last hop and the authorization code never
// reaches the client, while the server records a perfectly successful 303. It is the same
// failure the provider origins already exist to avoid, one step further along.
//
// The redirect must be one allowedRedirect has already matched against the client's
// registration. Taking it from the query string instead would let anyone put an origin of
// their choosing into the policy of a page they can make somebody else load.
//
// Enforcement is the consent document's policy, so that is the response that has to carry
// this; approve and deny carry it too because they answer the same navigation, and a
// response whose own policy forbids its own Location header is a trap for whoever reads it
// next. Both cases name the same validated URI, so neither widens anything the other did not.
func allowRedirectInFormAction(w http.ResponseWriter, redirect string) {
	source := formActionSource(redirect)
	if source == "" {
		return
	}
	policy := w.Header().Get("Content-Security-Policy")
	if policy == "" {
		return
	}

	directives := strings.Split(policy, ";")
	for i, directive := range directives {
		fields := strings.Fields(directive)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "form-action") {
			continue
		}
		for _, existing := range fields[1:] {
			if existing == source {
				return
			}
		}
		directives[i] = strings.TrimRight(directive, " ") + " " + source
		w.Header().Set("Content-Security-Policy", strings.Join(directives, ";"))
		return
	}
}

// formActionSource turns a registered redirect URI into a Content-Security-Policy source.
//
// Only the scheme, host and port: once a navigation has been redirected CSP stops matching
// paths at all, and the redirect hop is precisely the one this exists for — so a path would
// be ignored where it matters and merely be another thing to keep in step with the client's
// registration everywhere else. The callback also arrives carrying ?code= and &state=, which
// no path expression should have to care about.
//
// A private scheme becomes the scheme on its own, "cursor:" rather than
// "cursor://anysphere.cursor-mcp". A scheme-source is the shape CSP defines for a scheme the
// URL parser treats as opaque, and RFC 8252 clients put their identity in the scheme anyway.
// It is deliberately the coarser of the two: it admits every cursor:// URL, which is what
// naming a scheme means, and the alternative is a host expression browsers do not agree on.
func formActionSource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + ":"
}

func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri is not a URL")
	}
	// The consent screen presents this host to a human as the fact the client's own name is
	// not, so it has to be a host a human can read for what it is. A URI is supposed to carry
	// an international name in its punycode form anyway — that is what goes on the wire — and
	// a host spelled in another alphabet instead is a name that can be drawn to look like a
	// different one. Refusing it here means the screen never has to render one, and the
	// client is told the encoding to use.
	if (u.Scheme == "http" || u.Scheme == "https") && !asciiOnly(u.Hostname()) {
		return fmt.Errorf("redirect_uri host must be ASCII; register an international name in its punycode (xn--) form")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"):
		// Loopback HTTP is how desktop MCP clients receive the redirect.
		return nil
	case u.Scheme != "" && u.Scheme != "http" && !strings.Contains(u.Scheme, " ") && !executableScheme(u.Scheme):
		// A private scheme such as cursor:// — used by native clients, and permitted by
		// RFC 8252 for exactly that reason.
		return nil
	default:
		return fmt.Errorf("redirect_uri must be https, loopback http, or a private scheme")
	}
}

// executableScheme names the schemes that are never a redirect target and are always a way
// to run something. A denylist rather than a pattern because private-use schemes are
// legitimate here and have no shape worth requiring: real clients register cursor:// and
// vscode:// with no dot in them, so anything structural would refuse the honest cases while
// admitting the next dangerous scheme anyway.
func executableScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "javascript", "data", "vbscript", "file", "blob", "about", "filesystem", "view-source":
		return true
	default:
		return false
	}
}

func allowedRedirect(registered []string, candidate string) bool {
	for _, r := range registered {
		if r == candidate {
			return true
		}
	}
	return false
}

func redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, desc string) {
	if redirect == "" {
		http.Error(w, desc, http.StatusBadRequest)
		return
	}
	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, desc, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func capabilityStrings() []string {
	out := make([]string, len(mail.AllCapabilities))
	for i, c := range mail.AllCapabilities {
		out[i] = string(c)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
