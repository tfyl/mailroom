package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// DefaultOIDCScopes is what an issuer is asked for when nothing else is configured.
//
// Deliberately the three every OIDC provider implements. `groups` is *not* here: it is not a
// standard scope, and an issuer that does not know it rejects the whole authorization
// request rather than ignoring it. Google is one such issuer — its scopes_supported is
// exactly openid, email and profile — so asking for groups there fails every sign-in with a
// bare "invalid_scope" and no clue which scope was at fault.
var DefaultOIDCScopes = []string{oidc.ScopeOpenID, "profile", "email"}

// OIDC authenticates the operator against any compliant issuer: Authentik, Keycloak,
// Zitadel, Auth0, Okta, Entra, Google. Nothing here is provider-specific — configuration is
// an issuer, a client id and a secret.
type OIDC struct {
	id       string
	label    string
	issuer   string
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	sessions *Sessions
	secure   bool

	requiredGroup string
	requiredClaim string

	pending *pendingLogins
}

type OIDCOptions struct {
	// ID is the url-safe slug identifying this provider, used in its callback path and on
	// the login page. Several issuers can be configured at once, so it has to be stable.
	ID    string
	Label string

	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string

	// Scopes overrides DefaultOIDCScopes. Add "groups" only for issuers that implement it —
	// Authentik and Keycloak do, Google does not.
	Scopes []string

	RequiredGroup string
	RequiredClaim string // "key=value"

	Sessions      *Sessions
	SecureCookies bool
}

func NewOIDC(ctx context.Context, o OIDCOptions) (*OIDC, error) {
	provider, err := discover(ctx, o.Issuer)
	if err != nil {
		return nil, err
	}

	scopes := o.Scopes
	if len(scopes) == 0 {
		scopes = DefaultOIDCScopes
	}
	// A required group is unenforceable without a claim carrying groups, and on the issuers
	// that have one the scope is what produces it. Adding it here means configuring the
	// requirement is enough, rather than also having to know that.
	//
	// The same applies to a required *claim* naming groups: without the scope the claim is
	// simply absent, and an absent claim refuses everybody. That failure looks exactly like a
	// policy working correctly, which is the worst way for a misconfiguration to present.
	needsGroups := o.RequiredGroup != "" || strings.HasPrefix(o.RequiredClaim, "groups=")
	if needsGroups && !contains(scopes, "groups") {
		scopes = append(append([]string{}, scopes...), "groups")
	}

	id := o.ID
	if id == "" {
		id = "oidc"
	}

	return &OIDC{
		id:       id,
		label:    firstNonEmpty(o.Label, "Single sign-on"),
		issuer:   o.Issuer,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: o.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  o.RedirectURL,
			Scopes:       scopes,
		},
		sessions:      o.Sessions,
		secure:        o.SecureCookies,
		requiredGroup: o.RequiredGroup,
		requiredClaim: o.RequiredClaim,
		pending:       newPendingLogins(10 * time.Minute),
	}, nil
}

func (o *OIDC) ID() string { return o.id }

// CallbackPath is where this provider returns to, derived from the redirect URI it was
// configured with so the route and the URI registered at the issuer cannot drift apart.
func (o *OIDC) CallbackPath() string {
	if u, err := url.Parse(o.oauth.RedirectURL); err == nil && u.Path != "" {
		return u.Path
	}
	return "/auth/" + o.id + "/callback"
}
func (o *OIDC) Label() string { return o.label }
func (o *OIDC) Mode() string  { return "oidc" }

// Matches reports whether an id_token issuer belongs to this provider, ignoring a trailing
// slash — the same spelling difference discover() tolerates, which would otherwise make a
// session unroutable back to the provider that created it.
func (o *OIDC) Matches(issuer string) bool {
	return strings.TrimSuffix(issuer, "/") == strings.TrimSuffix(o.issuer, "/")
}

// discover fetches the issuer's configuration, tolerating a trailing-slash difference
// between what the operator configured and what the issuer advertises.
//
// Some providers — Authentik among them — return a discovery document whose issuer carries a
// trailing slash, and go-oidc compares issuers as exact strings. Left alone this surfaces as
// a baffling startup failure that every operator has to rediscover, so both spellings are
// tried before giving up. Validation is not disabled: the retry still requires an exact
// match against the alternative spelling.
func discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	provider, firstErr := oidc.NewProvider(ctx, issuer)
	if firstErr == nil {
		return provider, nil
	}

	alt := strings.TrimSuffix(issuer, "/")
	if alt == issuer {
		alt = issuer + "/"
	}
	if provider, err := oidc.NewProvider(ctx, alt); err == nil {
		return provider, nil
	}
	return nil, fmt.Errorf("discovering OIDC issuer %q (also tried %q): %w", issuer, alt, firstErr)
}

func (o *OIDC) Identify(r *http.Request) (Operator, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Operator{}, ErrNoSession
	}
	op, ok := o.sessions.Get(c.Value)
	if !ok {
		return Operator{}, ErrNoSession
	}
	return op, nil
}

// Authorize enforces the configured group. Without one, any account at the issuer could
// administer this instance — which is why the deployment documentation tells operators to
// set one, and says so loudest for Google, where "any account" means every Google account.
func (o *OIDC) Authorize(op Operator) error {
	if o.requiredGroup == "" {
		return nil
	}
	if contains(op.Groups, o.requiredGroup) {
		return nil
	}
	return ErrNotAuthorized
}

// StartLogin begins the authorization-code flow.
//
// Every request carries PKCE and a nonce. PKCE binds the authorization code to this exact
// attempt, so an intercepted code is useless to anyone else; the nonce binds the resulting
// id_token to it, so a token minted for a different flow cannot be replayed into this one.
// Neither is optional in OAuth 2.1, and both are cheap.
func (o *OIDC) StartLogin(w http.ResponseWriter, r *http.Request) bool {
	next := safeNext(firstNonEmpty(r.URL.Query().Get("next"), r.URL.Path))

	verifier := oauth2.GenerateVerifier()
	nonce, err := randomToken(16)
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return true
	}
	state, err := randomToken(24)
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return true
	}

	// Bound to this browser, not just to this attempt.
	//
	// PKCE and the nonce above bind the authorization code and the id_token to the flow.
	// Neither binds the flow to the browser that started it, and without that the callback
	// is a URL anyone may complete: an attacker signs in at the issuer as themselves, keeps
	// the callback instead of following it, and gets the victim to open it. It is a
	// top-level GET carrying no cookie, so SameSite does not apply. The victim's browser is
	// then signed in as the attacker — and the next mailbox they link, or credential they
	// paste, is stored in the attacker's account.
	//
	// The binder is what closes it: a value only this browser was given, which the callback
	// has to present alongside the state.
	binder, err := randomToken(24)
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return true
	}
	setLoginCookie(w, binder, o.secure, o.pending.ttl)

	o.pending.put(state, pendingLogin{Next: next, Verifier: verifier, Nonce: nonce, Binder: binder})

	http.Redirect(w, r, o.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
	), http.StatusSeeOther)
	return true
}

// Callback completes the authorization-code flow and creates a session.
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) (string, error) {
	q := r.URL.Query()

	// State first, before anything else in this URL is read — the issuer's error included.
	//
	// A callback matching no attempt this server started is a link somebody was sent, not a
	// sign-in that failed, and it should be answered as one. Reading the error first meant a
	// bare link could reach the rendering path with no state check of any kind behind it.
	//
	// Real refusals still land below: RFC 6749 §4.1.2.1 requires the issuer to return the
	// state whenever the authorization request carried one, and StartLogin always sends one.
	state := q.Get("state")
	attempt, ok := o.pending.take(state)
	if !ok {
		cause := errors.New("callback state matches no sign-in this server started")
		// Carried into the log rather than dropped: an issuer that omits the state on an
		// error response would otherwise leave the operator with nothing to go on.
		if code := q.Get("error"); code != "" {
			cause = fmt.Errorf("%w (the callback also carried error=%q error_description=%q)",
				cause, code, q.Get("error_description"))
		}
		return "", &LoginError{
			Message: "This sign-in link is no longer valid. Start again from the sign-in page.",
			Cause:   cause,
		}
	}

	// An issuer reports a refusal here rather than at the token endpoint, and which refusal
	// it was is worth knowing. The *code* is what says so, mapped through wording of our
	// own; error_description is free text chosen by whoever built the URL, and it goes to
	// the log instead of onto a page served from this instance's own origin.
	if code := q.Get("error"); code != "" {
		clearLoginCookie(w, o.secure)
		return "", &LoginError{
			Message: oauthErrorMessage(code),
			Cause: fmt.Errorf("identity provider returned error=%q error_description=%q",
				code, q.Get("error_description")),
		}
	}

	// The browser that started this attempt is the only one allowed to finish it. Cleared
	// either way: the attempt is spent, and leaving the cookie behind would only confuse the
	// next sign-in.
	binder, err := r.Cookie(loginCookie)
	clearLoginCookie(w, o.secure)
	if err != nil || subtle.ConstantTimeCompare([]byte(binder.Value), []byte(attempt.Binder)) != 1 {
		return "", &LoginError{
			Message: "This sign-in was started in a different browser. Start again from the sign-in page.",
			Cause:   errors.New("this sign-in was started in a different browser"),
		}
	}

	token, err := o.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(attempt.Verifier))
	if err != nil {
		return "", fmt.Errorf("exchanging authorization code: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		return "", fmt.Errorf("issuer returned no id_token")
	}
	idToken, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		return "", fmt.Errorf("verifying id_token: %w", err)
	}
	if idToken.Nonce != attempt.Nonce {
		return "", fmt.Errorf("id_token nonce does not match this sign-in attempt")
	}

	var claims struct {
		Email  string   `json:"email"`
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", fmt.Errorf("reading claims: %w", err)
	}

	op := Operator{
		Issuer:  idToken.Issuer,
		Subject: idToken.Subject,
		Email:   claims.Email,
		Name:    firstNonEmpty(claims.Name, claims.Email, idToken.Subject),
		Groups:  claims.Groups,
	}
	// Both of these are policy: the identity is real and this instance will not have it. One
	// message covers them because telling a refused visitor which group or claim would have
	// let them in is the thing Authorize is careful not to do; the log says which.
	if err := o.Authorize(op); err != nil {
		return "", &LoginError{Message: "Your account is not permitted to administer this instance.", Cause: err}
	}
	if o.requiredClaim != "" {
		if err := checkClaim(idToken, o.requiredClaim); err != nil {
			return "", &LoginError{Message: "Your account is not permitted to administer this instance.", Cause: err}
		}
	}

	session, err := o.sessions.Create(op)
	if err != nil {
		return "", err
	}
	setSessionCookie(w, session, o.secure, o.sessions.TTL())

	return safeNext(attempt.Next), nil
}

func (o *OIDC) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		o.sessions.Delete(c.Value)
	}
	clearSessionCookie(w, o.secure)
}

// checkClaim enforces a `key=value` requirement against the id_token.
//
// Claims are not all strings. `email_verified` is a boolean and `hd` is a string, and an
// implementation that only reads strings refuses every boolean claim as a mismatch — locking
// everyone out while looking like a policy decision. Comparison is therefore done on the
// claim's JSON rendering, with quotes stripped, so true, 42 and "example.com" all behave the
// way an operator writing `email_verified=true` expects.
func checkClaim(token *oidc.IDToken, requirement string) error {
	key, want, found := strings.Cut(requirement, "=")
	if !found {
		return fmt.Errorf("required claim must look like key=value")
	}

	var all map[string]json.RawMessage
	if err := token.Claims(&all); err != nil {
		return err
	}
	raw, ok := all[key]
	if !ok {
		return ErrNotAuthorized
	}

	// A list claim satisfies the requirement when it contains the value, which is how group
	// membership arrives from issuers that do not use a `groups` claim name.
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		if contains(list, want) {
			return nil
		}
		return ErrNotAuthorized
	}

	got := strings.Trim(string(raw), `"`)
	if got != want {
		return ErrNotAuthorized
	}
	return nil
}

// pendingLogins holds in-flight sign-in attempts: the state parameter mapped to the PKCE
// verifier, the nonce, and where the user was heading.
type pendingLogins struct {
	mu   sync.Mutex
	data map[string]pendingLogin
	ttl  time.Duration
}

type pendingLogin struct {
	Next     string
	Verifier string
	Nonce    string
	// Binder is echoed in a cookie on the browser that started this attempt, and has to come
	// back with the callback. See StartLogin for why state alone is not enough.
	Binder  string
	expires time.Time
}

func newPendingLogins(ttl time.Duration) *pendingLogins {
	return &pendingLogins{data: map[string]pendingLogin{}, ttl: ttl}
}

func (p *pendingLogins) put(state string, l pendingLogin) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.data {
		if now.After(v.expires) {
			delete(p.data, k)
		}
	}
	l.expires = now.Add(p.ttl)
	p.data[state] = l
}

// take consumes the attempt, so a replayed callback cannot mint a second session.
func (p *pendingLogins) take(state string) (pendingLogin, bool) {
	if state == "" {
		return pendingLogin{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.data[state]
	delete(p.data, state)
	if !ok || time.Now().After(l.expires) {
		return pendingLogin{}, false
	}
	return l, true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// safeNext sanitises where a completed sign-in sends somebody.
//
// The destination is allowed to be one thing: a rooted path on this instance, optionally
// with a query. That is stated positively rather than as a list of shapes to refuse, because
// the browser and this function have to agree on what a value means and only the browser
// gets a vote. `/\evil.com` is the example: every WHATWG implementation reads the backslash
// as a separator, so it navigates to //evil.com, and a check written as "does it start with
// //" sees an ordinary path. Anything that could carry an authority — a scheme, a host, a
// second leading slash, a backslash in any encoding — is not a path here.
//
// Judged on the decoded path, so %5C cannot smuggle a separator past a check on the raw
// string, and rebuilt from what was parsed rather than passed through, so what the redirect
// carries is what was inspected.
//
// It also refuses the login machinery itself: a user arriving at /auth/x/start has that
// recorded as where they were headed, so returning them there after a successful sign-in
// starts another sign-in, forever. The login page links straight to those paths, so this is
// the ordinary case rather than an edge one.
func safeNext(next string) string {
	const fallback = "/accounts"

	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Opaque != "" || u.Host != "" || u.User != nil {
		return fallback
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, `\`) {
		return fallback
	}
	if strings.HasPrefix(u.Path, "/auth/") || u.Path == "/login" {
		return fallback
	}

	cleaned := u.EscapedPath()
	if u.RawQuery != "" {
		cleaned += "?" + u.RawQuery
	}
	return cleaned
}
