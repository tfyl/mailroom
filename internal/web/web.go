// Package web serves the operator interface: mailboxes, grants, audit, and the consent
// screen where an operator decides what a client may do.
//
// Server-rendered HTML with no build step. The consent screen is the most security-sensitive
// page in the product, and it wants real form semantics, a session in an httpOnly cookie, and
// a content-security policy that can forbid script outright — all of which a bundled
// single-page app makes harder rather than easier.
package web

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	oauth2api "google.golang.org/api/oauth2/v2"
	"google.golang.org/api/option"

	"github.com/tfyl/mailroom/internal/app"
	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/mcp"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
	"github.com/tfyl/mailroom/internal/provider/microsoft"
	"github.com/tfyl/mailroom/internal/provider/zoho"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

//go:embed templates/*.html
var files embed.FS

type Server struct {
	store     *store.Store
	providers *app.Providers
	sealer    *secrets.Sealer
	operator  *auth.Registry
	signups   signup.Policy
	// holds is the other end of the held-action queue: internal/mcp writes to it when a
	// grant's mode says a privileged call must wait, and this is where its owner answers.
	holds     *held.Queue
	publicURL string
	secure    bool
	csrf      *csrf
	log       *slog.Logger

	pages map[string]*template.Template
	links *linkStore
}

func New(st *store.Store, providers *app.Providers, sealer *secrets.Sealer, operator *auth.Registry, holds *held.Queue, signups signup.Policy, publicURL string, log *slog.Logger) (*Server, error) {
	c, err := newCSRF()
	if err != nil {
		return nil, err
	}

	s := &Server{
		store: st, providers: providers, sealer: sealer,
		operator: operator, signups: signups, holds: holds, csrf: c, log: log,
		publicURL: strings.TrimSuffix(publicURL, "/"),
		// Cookies are marked Secure whenever the instance is reachable over HTTPS. Deriving
		// it from the public URL rather than from the request is deliberate: behind a
		// terminating proxy the request itself arrives over plain HTTP.
		secure: strings.HasPrefix(publicURL, "https://"),
		pages:  map[string]*template.Template{},
		links:  newLinkStore(10 * time.Minute),
	}
	for _, name := range []string{"accounts", "grants", "revoke", "grant_edit", "grant_widen",
		"audit", "held", "consent", "login", "invites", "refused"} {
		// mcp_endpoint.html is not a page: it is the block two pages draw the endpoint
		// from, so it is parsed alongside every page rather than owned by one of them.
		t, err := template.New(name+".html").Funcs(templateFuncs).ParseFS(files,
			"templates/layout.html", "templates/mcp_endpoint.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	return s, nil
}

var templateFuncs = template.FuncMap{"mailAddress": mailAddress}

// mailAddress renders an address with Cloudflare's opt-out markers around it.
//
// Cloudflare's Email Address Obfuscation, on by default for a zone, rewrites anything in the
// HTML that looks like an address to the literal string "[email protected]" and injects a
// script to decode it in the browser. That leaves the operator looking at a placeholder where
// the address of their own mailbox should be.
//
// Whether the decoder runs is not something this server can answer. It used to be: the policy
// carried no script-src at all, so the injected script was refused and the placeholder was
// permanent. The policy is script-src 'self' now, and Cloudflare serves that decoder from a
// /cdn-cgi/ path on this same origin, so it may well execute — which makes the rendered result
// depend on a zone setting nobody here controls. Not knowing is reason enough: the markers stop
// the rewrite from happening rather than betting on what undoes it.
//
// <!--email_off--> is Cloudflare's documented exclusion, and the markers have to be written
// through a function returning template.HTML because html/template strips comments written
// literally in a template — silently, so the fix would appear to be in place and do nothing.
// The address itself is escaped, since it comes from a mail provider rather than from us.
//
// This lives here rather than in an edge rule so that it holds for anyone self-hosting behind
// Cloudflare, without their having to know the feature exists.
func mailAddress(address string) template.HTML {
	return template.HTML("<!--email_off-->" + template.HTMLEscapeString(address) + "<!--/email_off-->")
}

// Router is the part of *http.ServeMux this package registers against.
//
// Narrowed to an interface so that the route table is something a test can walk rather than
// only something a server can serve. routes_test.go registers the whole browser surface into
// a recorder and asserts that every mutating route on it refuses a request carrying no CSRF
// token. POST /logout was registered bare for as long as it existed, and nothing short of
// reading twenty near-identical lines one at a time would have found it.
type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Routes registers the browser surface. Every path here sits behind operator authentication
// except the login form itself.
func (s *Server) Routes(mux Router, oauth *oauthsrv.Server) {
	// Every browser path runs through both: auth.Require proves who is signed in, and
	// withUser turns that identity into the user row everything else is scoped by.
	guard := func(h http.HandlerFunc) http.Handler {
		return auth.Require(s.operator, s.withUser(h))
	}

	// post registers a mutating route, and is the only way this file registers one. The
	// method and both wrappers live in here rather than at each call site, so adding a write
	// means getting the CSRF check whether or not you remembered to ask for it. That is the
	// half a reviewer cannot check by eye: every registration below looks like every other
	// registration below, which is exactly how one of them came to be missing a wrapper.
	post := func(path string, h http.HandlerFunc) {
		mux.Handle("POST "+path, guard(s.csrf.protect(h)))
	}

	// Outside the guard, and it has to be: the sign-in page is served to somebody who has
	// not signed in, and a login screen that cannot style itself is the one page where that
	// matters most. There is nothing in a stylesheet to protect.
	mux.HandleFunc("GET "+stylesheetURL, s.serveStylesheet)
	// The script goes beside it, outside the guard for the same reason and with less at
	// stake: it holds no data and reads none. It is enhancements over markup the server has
	// already rendered, so anybody who can fetch it learns nothing the page did not tell
	// them — and the login page links it as every page does.
	mux.HandleFunc("GET "+scriptURL, s.serveScript)

	mux.Handle("GET /{$}", http.RedirectHandler("/accounts", http.StatusSeeOther))
	mux.Handle("GET /accounts", guard(s.accounts))
	post("/accounts/link/google", s.linkGoogle)
	mux.Handle("GET /accounts/link/google/callback", guard(s.linkGoogleCallback))
	post("/accounts/link/zoho", s.linkZoho)
	mux.Handle("GET /accounts/link/zoho/callback", guard(s.linkZohoCallback))
	post("/accounts/link/microsoft", s.linkMicrosoft)
	mux.Handle("GET /accounts/link/microsoft/callback", guard(s.linkMicrosoftCallback))
	post("/accounts/link/imap", s.linkIMAP)
	post("/accounts/rename", s.rename)
	post("/accounts/unlink", s.unlink)

	mux.Handle("GET /invites", guard(s.invites))
	post("/invites/create", s.createInvite)
	post("/invites/revoke", s.revokeInvite)

	// Redeeming happens on the next sign-in, so this cannot sit behind the guard: whoever
	// follows an invite link has no account yet, which is the entire point of holding one.
	mux.HandleFunc("GET /invite/{code}", s.acceptInvite)

	mux.Handle("GET /grants", guard(s.grants))
	mux.Handle("GET /grants/edit", guard(s.editGrantForm))
	post("/grants/edit", s.editGrant)
	post("/grants/revoke", s.revokeGrant)
	post("/grants/remove", s.removeGrant)
	post("/grants/remove-all", s.removeRevokedGrants)
	mux.Handle("GET /audit", guard(s.audit))

	// The held queue. Approving is the one place in this UI where pressing a button sends
	// mail, so it is a POST behind the CSRF guard like every other write, and both endpoints
	// scope every store call to the signed-in operator.
	mux.Handle("GET /held", guard(s.heldQueue))
	post("/held/approve", s.approveHeld)
	post("/held/decline", s.declineHeld)

	// The consent screen must be seen by a human, so it is behind operator auth even though
	// the rest of the OAuth endpoints are deliberately not.
	mux.Handle("GET /authorize", guard(oauth.Authorize))
	post("/authorize/approve", oauth.Approve)
	post("/authorize/deny", oauth.Deny)
	// Select-all is a submission of the consent form like any other, so it is behind the
	// same guard and the same CSRF check. It decides nothing — it re-renders the page — but
	// a form that anyone could post to would still be a way to redraw somebody's consent
	// screen with every box ticked.
	post("/authorize/reselect", oauth.Reselect)

	// The login page lists whatever is configured. It is registered whenever there is an
	// interactive method at all, since StartLogin sends people here as soon as there is more
	// than one and a single-method instance still needs it for a direct visit.
	if len(s.operator.Methods()) > 0 {
		mux.HandleFunc("GET /login", s.loginForm)
	}

	// One start and one callback per identity provider. Neither can sit behind the guard
	// that requires a session, because they are how a session is obtained.
	for _, provider := range s.operator.OIDCs() {
		p := provider
		mux.HandleFunc("GET /auth/"+p.ID()+"/start", func(w http.ResponseWriter, r *http.Request) {
			p.StartLogin(w, r)
		})
		mux.HandleFunc("GET "+p.CallbackPath(), func(w http.ResponseWriter, r *http.Request) {
			next, err := p.Callback(w, r)
			if err != nil {
				// The whole failure, including whatever the issuer said about it, goes
				// here and only here. It used to go onto the page as well, which meant a
				// query parameter in a link anybody could write ended up rendered above a
				// real sign-in form on this instance's own domain.
				s.log.Warn("oidc sign-in failed", "provider", p.ID(), "err", err)
				// The login page again, so a person can simply try another method — but
				// with 401, so a failed sign-in is not indistinguishable from a successful
				// one to anything reading status codes.
				w.WriteHeader(http.StatusUnauthorized)
				s.render(w, r, "login", "Sign in", "", map[string]any{
					"Methods": s.operator.Methods(),
					"Next":    "",
					// auth.LoginMessage is wording chosen in that package, never text
					// from the issuer or from the URL.
					"Error": auth.LoginMessage(err),
				})
				return
			}
			http.Redirect(w, r, next, http.StatusSeeOther)
		})
	}

	// Signing out is a write, and it goes through post() like every other write. It did not
	// used to: it was registered bare, so any page anywhere could end an operator's session
	// by posting a form at it. Nothing is destroyed by a forced sign-out, which is why this
	// survived — but a mutating route outside the check is a mutating route outside the
	// check, and the next one might not be this harmless.
	post("/logout", func(w http.ResponseWriter, r *http.Request) {
		s.operator.Logout(w, r)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// SecurityHeaders applies a policy that admits one script and one stylesheet, both of them
// files this server serves, and nothing else at all.
//
// script-src is 'self'. It used to be absent — default-src 'none' denied script outright,
// because the UI shipped none — and the directive is narrow rather than open now that one
// file exists: no 'unsafe-inline', so an injected <script> block or an on* attribute is
// still dead markup and still cannot rewrite what a button appears to do; no 'unsafe-eval';
// no origin but this one, so there is no CDN to compromise and nothing to fetch from a host
// this deployment does not control. The one file it admits is /static/app.<digest>.js, which
// only ever enhances controls that already work without it — internal/web/confirm_test.go
// keeps the inline half of that true, and docs/ui.md has the rest of the rule.
//
// style-src is 'self' rather than 'unsafe-inline' because the stylesheet is a file this
// server serves and no template carries a style attribute. That is worth keeping: an
// injection that reached the page could otherwise still restyle it, and restyling a consent
// screen — moving Deny off the fold, dressing Approve as something else — is an attack on
// the one screen in this product where what a control does has to be obvious.
//
// img-src is data: alone, and it is there for two marks: the tick inside a checkbox and the
// chevron on a select, both of which the design system draws from an SVG data URI. Under
// default-src 'none' a browser refuses them without a word — the control renders, and simply
// has no mark on it, which is a bad way for a tick box to fail on a consent screen. data: is
// the narrowest thing that admits them: it names no origin, so nothing here can fetch an
// image, and there is no remote host to reach whatever ends up on the page.
//
// formActions names the provider consent screens a linking flow hands off to. They have to
// be listed: `form-action` governs the entire redirect chain that follows a form submission,
// not just its immediate target, so `'self'` alone blocks the redirect to Google or Zoho.
// The browser refuses it and the server sees a perfectly successful response, which makes it
// a genuinely confusing failure. Deriving the list from the configured providers keeps the
// directive as narrow as it can be while letting linking work.
func SecurityHeaders(formActions []string, next http.Handler) http.Handler {
	formAction := strings.Join(append([]string{"'self'"}, formActions...), " ")
	csp := "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; form-action " +
		formAction + "; frame-ancestors 'none'; base-uri 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withUser resolves the authenticated operator to a user row and puts it on the request
// context, creating the row the first time somebody signs in.
//
// This is where an identity from any of the three auth modes becomes an owner. The first
// user ever to sign in also adopts any data left unowned by an install that predates
// multi-user support, which is logged because silently reassigning somebody's mailboxes
// would be a surprising thing to do quietly.
func (s *Server) withUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}

		resolved, adopted, err := s.store.EnsureUser(r.Context(), user.User{
			Issuer:  op.Issuer,
			Subject: op.Subject,
			Email:   op.Email,
			Name:    op.Name,
		}, store.Admission{Policy: s.signups, InviteCode: inviteFrom(r)})
		switch {
		case errors.Is(err, store.ErrSignupRefused):
			// Authenticated, and still not a user here. Ending the session as well stops the
			// next request looping straight back through a sign-in that cannot succeed.
			s.log.Info("refused a new account", "issuer", op.Issuer, "policy", s.signups.Mode)
			s.operator.Logout(w, r)
			s.clearInvite(w)
			w.WriteHeader(http.StatusForbidden)
			s.render(w, r, "refused", "Not accepting new accounts", "", map[string]any{
				"Policy":      s.signups.Mode,
				"NeedsInvite": s.signups.Mode == signup.Invite,
				// Offered only where there is a sign-in page to offer. Behind a proxy that
				// authenticates for us there is none, and a link to one would be a dead end
				// shown to the one person on the instance least able to work out why.
				"CanSignIn": len(s.operator.Methods()) > 0,
			})
			return

		case err != nil:
			s.log.Error("resolving the signed-in user failed", "err", err)
			http.Error(w, "could not identify you", http.StatusInternalServerError)
			return
		}
		// The invite has done its work, or was never needed. Either way it should not linger
		// in the browser waiting to be spent on a different identity.
		s.clearInvite(w)
		if adopted {
			s.log.Info("adopted pre-existing mailboxes and grants",
				"user", resolved.ID, "issuer", resolved.Issuer,
				"note", "data from before multi-user support now belongs to the first user to sign in")
		}

		next(w, r.WithContext(user.NewContext(r.Context(), resolved)))
	}
}

// currentUser returns the signed-in user. Handlers behind guard always have one.
func currentUser(r *http.Request) (user.User, bool) {
	return user.FromContext(r.Context())
}

// sourceRepo is where this program's source lives, and it is the only host any page here
// links to.
const sourceRepo = "https://github.com/tfyl/mailroom"

// stampedRevision matches the two shapes the release workflow stamps into a build: a version
// tag, or the commit a build of main was made from. Anything else — "dev" from an unstamped
// build, or whatever a self-hoster passes to -X — matches nothing and is treated as naming
// no particular revision.
var stampedRevision = regexp.MustCompile(`^(v[0-9][0-9A-Za-z.+-]*|[0-9a-f]{7,40})$`)

// sourceURL resolves the offer in the footer to the source of the build that is answering,
// rather than to whatever the default branch says today.
//
// Section 13 asks for the *corresponding* source, and a repository root stops corresponding
// the moment somebody pushes to it: an operator running last month's image and reading this
// month's main is not being shown what they are running. A revision the build named itself
// is, and GitHub resolves both shapes of it as a tree.
//
// A build that names no revision falls back to the repository, which is as precise an offer
// as it can honestly make. Guessing a path out of an unrecognised version string would
// produce a 404, and a link that goes nowhere is a worse offer than a general one.
func sourceURL(version string) string {
	rev := strings.TrimPrefix(version, "main-")
	if !stampedRevision.MatchString(rev) {
		return sourceRepo
	}
	return sourceRepo + "/tree/" + rev
}

// Resolved once: mcp.Version is fixed at link time, so there is nothing per-request about it.
var sourceOffer = sourceURL(mcp.Version)

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title, nav string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Title"] = title
	data["Nav"] = nav
	// The digest is in the name, so this changes whenever the stylesheet does and every page
	// asks for the file it was built against.
	data["Stylesheet"] = stylesheetURL
	// The same for the script, which every page links and most pages make no use of. It
	// enhances markup that already works without it, so a page that links it and carries no
	// data-enhance attribute behaves identically either way.
	data["Script"] = scriptURL
	// The AGPL section 13 offer, which the layout renders on every page. Both halves are
	// needed: the link alone would point at source that has moved on, and the version alone
	// would name a build with nowhere to get it from. See sourceURL and layout.html.
	data["Version"] = mcp.Version
	data["SourceURL"] = sourceOffer
	data["CSRF"] = s.csrf.token(w, r, s.secure)
	// The address an MCP client is pointed at, built from this instance's own public URL so
	// a differently deployed instance shows its own rather than a documented example.
	data["MCPURL"] = s.publicURL + "/mcp"
	// Shown in the header on every page. On a shared instance, knowing which account you are
	// acting as is the difference between linking a mailbox to the right identity and the
	// wrong one.
	if me, ok := currentUser(r); ok {
		data["SignedInAs"] = me.Display()
		// Only the owner can issue invites, so only the owner is shown the way to.
		if owner, err := s.store.IsOwner(r.Context(), me.ID); err == nil {
			data["IsOwner"] = owner
		}
		// How many privileged actions are waiting, on every page rather than only on the one
		// that lists them. A queue whose whole purpose is that a person answers it is worth
		// one indexed count per render: an operator who has to remember to go and look at it
		// will find mail that has been waiting three days, which is the failure this mode has
		// instead of sending something it should not have.
		if n, err := s.holds.Count(r.Context(), me.ID); err == nil {
			data["HeldCount"] = n
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("rendering page failed", "page", page, "err", err)
	}
}

// --- Mailboxes ---

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	s.renderAccounts(w, r, http.StatusOK, nil)
}

// renderAccounts draws the mailboxes page, optionally carrying a form's own error and the
// values already typed into it.
//
// The IMAP form posts here rather than to a page of its own, so a rejected password comes
// back with everything except the password still filled in. Redrawing the whole page is what
// makes that possible, and it is the reason this is not simply the accounts handler.
func (s *Server) renderAccounts(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	me, _ := currentUser(r)

	accounts, err := s.store.ListAccounts(r.Context(), me.ID)
	if err != nil {
		http.Error(w, "could not load mailboxes", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Accounts":       accounts,
		"GoogleReady":    s.providers.GoogleOAuth() != nil,
		"ZohoReady":      s.providers.ZohoOAuth() != nil,
		"MicrosoftReady": s.providers.MicrosoftOAuth() != nil,
		"User":           me,
		// Signing in and granting access to mail are deliberately separate steps, so a user
		// with no mailboxes yet has arrived somewhere that looks empty. Say what to do next
		// rather than leaving them to work it out.
		"FirstRun":  len(accounts) == 0,
		"IMAP":      imapForm{TLS: true},
		"NoSending": r.URL.Query().Get("sending") == "off",
		// Which provider's disclosure starts open, and which field a rejected form is drawn
		// against. Both are declared here rather than left absent, because a template
		// comparing an absent key against a typed value fails at execution rather than
		// reading as empty.
		"LinkOpen":       firstUsableProvider(s.providers, len(accounts) == 0),
		"IMAPErrorField": "",
		"RenameAt":       mail.AccountID(""),
		"RenameAlias":    "",
	}
	// Matched against the mailboxes actually linked, so the confirmation names something real
	// rather than repeating whatever the query string said.
	for _, a := range accounts {
		if a.Alias == r.URL.Query().Get("linked") {
			data["Linked"] = a.Alias
		}
		if a.Alias == r.URL.Query().Get("renamed") {
			data["Renamed"] = a.Alias
		}
	}
	for k, v := range extra {
		data[k] = v
	}
	// A rejected rename belongs beside the field it was typed into, so it is attached to the
	// row it names. If it names no row of theirs — a bad id and a bad alias arriving together,
	// which the store would have refused a moment later — the page shows it at the top rather
	// than dropping it.
	if id, ok := data["RenameAt"].(mail.AccountID); ok && id != "" {
		data["RenameAt"] = mail.AccountID("")
		for _, a := range accounts {
			if a.ID == id {
				data["RenameAt"] = id
			}
		}
	}

	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "accounts", "Mailboxes", "accounts", data)
}

// firstUsableProvider names the linking form the page should open by itself, or nothing.
//
// Somebody with mailboxes already came here to look at them, and four closed rows is the
// answer; opening one would push the list they came for off the screen. Somebody with none
// came here to link one, so the chooser opens the first provider this instance can actually
// use — IMAP when none of the OAuth clients is configured, since it is the one that always
// works.
func firstUsableProvider(p *app.Providers, firstRun bool) string {
	switch {
	case !firstRun:
		return ""
	case p.GoogleOAuth() != nil:
		return "google"
	case p.MicrosoftOAuth() != nil:
		return "microsoft"
	case p.ZohoOAuth() != nil:
		return "zoho"
	default:
		return "imap"
	}
}

func (s *Server) linkGoogle(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.GoogleOAuth()
	if conf == nil {
		http.Error(w, "Google linking is not configured on this instance", http.StatusPreconditionFailed)
		return
	}

	alias, err := mail.ParseAlias(r.FormValue("alias"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	me, _ := currentUser(r)
	state := ids.New("link")
	s.links.put(state, linkAttempt{Owner: me.ID, Alias: alias})

	// Offline access with a forced prompt: without it Google returns no refresh token on a
	// re-link, and the mailbox would work until the first access token expired.
	url := conf.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"))
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (s *Server) linkGoogleCallback(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.GoogleOAuth()
	if conf == nil {
		http.Error(w, "Google linking is not configured", http.StatusPreconditionFailed)
		return
	}

	// The state is claimed against the signed-in user, not merely looked up. This is a
	// top-level GET, so SameSite=Lax sends whatever session the browser holds, and an
	// attempt started by somebody else must not complete into this account.
	me, _ := currentUser(r)
	alias, ok := s.links.take(r.URL.Query().Get("state"), me.ID)
	if !ok {
		http.Error(w, "this linking attempt expired; start again from the mailboxes page", http.StatusBadRequest)
		return
	}

	token, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "Google declined the authorization: "+err.Error(), http.StatusBadGateway)
		return
	}
	if token.RefreshToken == "" {
		http.Error(w, "Google returned no refresh token. Remove mailroom from your Google "+
			"account's third-party access and link again.", http.StatusBadGateway)
		return
	}

	address, err := googleAddress(r.Context(), conf, token)
	if err != nil {
		http.Error(w, "could not read the mailbox address: "+err.Error(), http.StatusBadGateway)
		return
	}

	account := mail.Account{
		ID:       mail.AccountID(ids.Account()),
		Alias:    alias,
		Address:  address,
		Provider: mail.ProviderGmail,
		Status:   mail.StatusLinked,
	}
	sealed, err := s.sealer.SealString(token.RefreshToken, string(account.ID))
	if err != nil {
		http.Error(w, "could not seal the credential", http.StatusInternalServerError)
		return
	}
	if err := s.store.LinkAccount(r.Context(), me.ID, account, sealed, strings.Join(app.GmailScopes, " ")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	s.log.Info("mailbox linked", "alias", alias, "provider", "gmail")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func googleAddress(ctx context.Context, conf *oauth2.Config, token *oauth2.Token) (string, error) {
	svc, err := oauth2api.NewService(ctx, option.WithTokenSource(conf.TokenSource(ctx, token)))
	if err != nil {
		return "", err
	}
	info, err := svc.Userinfo.Get().Do()
	if err != nil {
		return "", err
	}
	return info.Email, nil
}

// zohoScopes is the scope string Zoho wants. Zoho separates scopes with commas where OAuth 2
// separates them with spaces, and the library joins Config.Scopes with a space — so the
// authorization request carries a scope Zoho reads as one long unknown name, and refuses.
// The configured slice stays a slice: it is the honest record of what was asked for.
func zohoScopes(conf *oauth2.Config) string { return strings.Join(conf.Scopes, ",") }

func (s *Server) linkZoho(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.ZohoOAuth()
	if conf == nil {
		http.Error(w, "Zoho linking is not configured on this instance", http.StatusPreconditionFailed)
		return
	}

	alias, err := mail.ParseAlias(r.FormValue("alias"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	me, _ := currentUser(r)
	state := ids.New("link")
	s.links.put(state, linkAttempt{Owner: me.ID, Alias: alias})

	// Offline access with a forced prompt, for the same reason as Google and then some. Zoho
	// issues a refresh token only for an offline grant, and only alongside a consent somebody
	// actually sees: an authorization that rides an existing consent comes back with an access
	// token and nothing else. Omitting either parameter therefore produces a mailbox that
	// works for an hour and then cannot refresh — which surfaces as a credential error long
	// after the linking that caused it.
	authURL := conf.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("scope", zohoScopes(conf)))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (s *Server) linkZohoCallback(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.ZohoOAuth()
	if conf == nil {
		http.Error(w, "Zoho linking is not configured", http.StatusPreconditionFailed)
		return
	}

	// Claimed against the signed-in user rather than merely looked up, for the reason spelled
	// out on linkAttempt: this is a top-level GET and arrives with whatever session the
	// browser holds, so an attempt somebody else started must not complete into this account.
	me, _ := currentUser(r)
	alias, ok := s.links.take(r.URL.Query().Get("state"), me.ID)
	if !ok {
		http.Error(w, "this linking attempt expired; start again from the mailboxes page", http.StatusBadRequest)
		return
	}

	token, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "Zoho declined the authorization: "+err.Error(), http.StatusBadGateway)
		return
	}
	if token.RefreshToken == "" {
		http.Error(w, "Zoho returned no refresh token, so this mailbox could not stay linked "+
			"beyond the first hour. Remove mailroom from the connected applications in your "+
			"Zoho account and link again.", http.StatusBadGateway)
		return
	}

	address, err := zoho.PrimaryAddress(r.Context(), s.providers.ZohoRegion(), conf.TokenSource(r.Context(), token))
	switch {
	case errors.Is(err, mail.ErrNeedsReauth):
		// The provider reports a refused token as "re-link required", which is the right words
		// for a mailbox that was working and stopped and the wrong ones for a consent granted
		// two seconds ago. Nothing has expired: Zoho refused the token it had just issued, and
		// by far the likeliest reason is that the mailbox lives in a different data centre
		// from the one this instance is configured for.
		http.Error(w, "Zoho refused the access token it had just issued. Check that the "+
			"mailbox is in the data centre this instance is configured for.", http.StatusBadGateway)
		return
	case err != nil:
		http.Error(w, "could not read the mailbox address: "+err.Error(), http.StatusBadGateway)
		return
	}

	account := mail.Account{
		ID:       mail.AccountID(ids.Account()),
		Alias:    alias,
		Address:  address,
		Provider: mail.ProviderZoho,
		Status:   mail.StatusLinked,
	}
	sealed, err := s.sealer.SealString(token.RefreshToken, string(account.ID))
	if err != nil {
		http.Error(w, "could not seal the credential", http.StatusInternalServerError)
		return
	}
	if err := s.store.LinkAccount(r.Context(), me.ID, account, sealed, zohoScopes(conf)); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	s.log.Info("mailbox linked", "alias", alias, "provider", "zoho")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func (s *Server) linkMicrosoft(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.MicrosoftOAuth()
	if conf == nil {
		http.Error(w, "Microsoft linking is not configured on this instance", http.StatusPreconditionFailed)
		return
	}

	alias, err := mail.ParseAlias(r.FormValue("alias"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	me, _ := currentUser(r)
	state := ids.New("link")
	s.links.put(state, linkAttempt{Owner: me.ID, Alias: alias})

	// offline_access is what makes this a mailbox rather than an hour or so of one: the
	// Microsoft identity platform returns a refresh token only when that scope is among the
	// ones consented to. It travels in the ordinary scope list, which app.Providers sets from
	// microsoft.Scopes, so there is no separate parameter for it the way Google has one.
	//
	// The forced prompt is not, unlike Google's and Zoho's, needed to get that refresh token:
	// Microsoft returns one whenever offline_access was granted, consent screen or not. It is
	// here because a re-link that silently rides a consent granted earlier grants whatever
	// *that* consent covered — so an instance that has since started asking for more, or that
	// is re-linking a mailbox precisely because something was missing, would get the old
	// answer back and no indication of it. Showing the screen makes what is being granted the
	// thing the operator just looked at.
	authURL := conf.AuthCodeURL(state,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("response_mode", "query"))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (s *Server) linkMicrosoftCallback(w http.ResponseWriter, r *http.Request) {
	conf := s.providers.MicrosoftOAuth()
	if conf == nil {
		http.Error(w, "Microsoft linking is not configured", http.StatusPreconditionFailed)
		return
	}

	// Claimed against the signed-in user rather than merely looked up, for the reason spelled
	// out on linkAttempt: this is a top-level GET and arrives with whatever session the
	// browser holds, so an attempt somebody else started must not complete into this account.
	me, _ := currentUser(r)
	alias, ok := s.links.take(r.URL.Query().Get("state"), me.ID)
	if !ok {
		http.Error(w, "this linking attempt expired; start again from the mailboxes page", http.StatusBadRequest)
		return
	}

	token, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "Microsoft declined the authorization: "+err.Error(), http.StatusBadGateway)
		return
	}
	if token.RefreshToken == "" {
		http.Error(w, "Microsoft returned no refresh token, so this mailbox would stop working "+
			"as soon as the access token expired, about an hour from now. The app registration "+
			"must include the offline_access delegated permission; add it, remove mailroom "+
			"from https://myaccount.microsoft.com/consent, and link again.", http.StatusBadGateway)
		return
	}

	address, err := microsoft.PrimaryAddress(r.Context(), conf.TokenSource(r.Context(), token))
	switch {
	case errors.Is(err, mail.ErrNeedsReauth):
		// The provider reports a refused token as "re-link required", which is the right words
		// for a mailbox that was working and stopped and the wrong ones for a consent granted
		// two seconds ago. Nothing has expired: Graph refused the token Microsoft had just
		// issued, and the likeliest reason is an app registration whose API permissions do not
		// include the Graph scopes this asked for.
		http.Error(w, "Microsoft Graph refused the access token that had just been issued. "+
			"Check that the app registration grants the delegated Graph permissions "+
			"mailroom requests.", http.StatusBadGateway)
		return
	case err != nil:
		http.Error(w, "could not read the mailbox address: "+err.Error(), http.StatusBadGateway)
		return
	}

	account := mail.Account{
		ID:       mail.AccountID(ids.Account()),
		Alias:    alias,
		Address:  address,
		Provider: mail.ProviderMicrosoft,
		Status:   mail.StatusLinked,
	}
	sealed, err := s.sealer.SealString(token.RefreshToken, string(account.ID))
	if err != nil {
		http.Error(w, "could not seal the credential", http.StatusInternalServerError)
		return
	}
	if err := s.store.LinkAccount(r.Context(), me.ID, account, sealed, strings.Join(conf.Scopes, " ")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	s.log.Info("mailbox linked", "alias", alias, "provider", "microsoft")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// imapForm is what the operator typed, kept so a refused attempt is corrected rather than
// retyped. The password is deliberately absent: it is not going back out in a response.
type imapForm struct {
	Alias    string
	Address  string
	Host     string
	Username string
	SMTPHost string
	SMTPFrom string
	TLS      bool
}

// linkIMAP attaches an IMAP mailbox from the mailboxes page, the browser equivalent of
// `mailroom link-imap`.
//
// It is the only linking path that needs nothing registered anywhere: with an app password,
// a Gmail mailbox arrives here without an OAuth client, a consent screen or a Console. The
// two exist side by side because the audiences differ — the command suits a deployment being
// finished from a shell, and this suits somebody who has just signed in and wants their mail
// attached.
func (s *Server) linkIMAP(w http.ResponseWriter, r *http.Request) {
	form := imapForm{
		Alias:    strings.TrimSpace(r.FormValue("alias")),
		Address:  strings.TrimSpace(r.FormValue("address")),
		Host:     strings.TrimSpace(r.FormValue("host")),
		Username: strings.TrimSpace(r.FormValue("username")),
		SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")),
		SMTPFrom: strings.TrimSpace(r.FormValue("smtp_from")),
		TLS:      r.FormValue("insecure") == "",
	}
	// An app password is pasted with the spaces Google displays it with, and those spaces are
	// not part of it.
	password := strings.Join(strings.Fields(r.FormValue("password")), "")

	// field names the input the message belongs under, so a rejected submit is answered where
	// it was typed rather than by a banner at the top of a page it has to be matched back to
	// by eye. An empty one is a failure that belongs to the form as a whole.
	refuse := func(status int, field, msg string) {
		s.renderAccounts(w, r, status, map[string]any{
			"IMAP": form, "IMAPError": msg, "IMAPErrorField": field, "LinkOpen": "imap",
		})
	}
	alias, err := mail.ParseAlias(form.Alias)
	if err != nil {
		refuse(http.StatusBadRequest, "alias", capitalise(err.Error())+": it is how grants and tools will refer to this mailbox.")
		return
	}
	form.Alias = alias

	switch {
	case form.Address == "":
		refuse(http.StatusBadRequest, "address", "An address is required. It is what the mailbox is displayed as, and the default sender.")
		return
	case form.Host == "":
		refuse(http.StatusBadRequest, "host", "An IMAP server is required, as host:port.")
		return
	case password == "":
		refuse(http.StatusBadRequest, "password", "A password is required. For Gmail this is a 16-character app password rather than your account password.")
		return
	}

	account := mail.Account{
		ID:       mail.AccountID(ids.Account()),
		Alias:    form.Alias,
		Address:  form.Address,
		Provider: mail.ProviderIMAP,
		Status:   mail.StatusLinked,
	}
	cfg := imapprovider.Config{
		Host:     form.Host,
		Username: cmp.Or(form.Username, form.Address),
		Password: password,
		TLS:      form.TLS,
		SMTPHost: form.SMTPHost,
		SMTPFrom: cmp.Or(form.SMTPFrom, form.Address),
	}

	// Connecting before storing turns a typo into an error on this page rather than a mailbox
	// that exists, looks linked, and fails on first use.
	provider, err := imapprovider.New(r.Context(), account, cfg)
	switch {
	case errors.Is(err, mail.ErrNeedsReauth):
		// The provider reports a rejected login as "re-link required", which is the right
		// words for a mailbox that was working and stopped. Here there is nothing to
		// re-link: this is the first attempt and the credentials are simply wrong.
		refuse(http.StatusBadRequest, "password", cfg.Host+" rejected the credentials for "+cfg.Username+
			". For Gmail this is a 16-character app password, which needs 2-Step Verification "+
			"switched on. It is not your account password.")
		return
	case err != nil:
		refuse(http.StatusBadGateway, "host", "Could not reach "+cfg.Host+": "+err.Error())
		return
	}
	_ = provider.Close()

	blob, err := json.Marshal(cfg)
	if err != nil {
		refuse(http.StatusInternalServerError, "", "Could not prepare the credential for storage.")
		return
	}
	sealed, err := s.sealer.SealString(string(blob), string(account.ID))
	if err != nil {
		refuse(http.StatusInternalServerError, "", "Could not seal the credential.")
		return
	}
	me, _ := currentUser(r)
	if err := s.store.LinkAccount(r.Context(), me.ID, account, sealed, ""); err != nil {
		// The one that actually happens is an alias already taken, which is the alias field's
		// problem and nothing else's.
		refuse(http.StatusConflict, "alias", capitalise(err.Error())+".")
		return
	}

	s.log.Info("mailbox linked", "alias", account.Alias, "provider", "imap")
	next := "/accounts?linked=" + url.QueryEscape(account.Alias)
	if cfg.SMTPHost == "" {
		next += "&sending=off"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// rename changes a mailbox's alias.
//
// Errors re-render the accounts page rather than replacing it with a bare error, because the
// form being corrected lives on that page: a 409 text page would lose both the list and the
// name just typed. The refusal carries the mailbox it is about, so the page can open that
// row's disclosure with the rejected name still in the field rather than saying "not renamed"
// above a list of five and leaving the reader to work out which one.
func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	id := mail.AccountID(r.FormValue("id"))

	refuse := func(status int, msg string) {
		s.renderAccounts(w, r, status, map[string]any{
			"RenameError": msg, "RenameAt": id, "RenameAlias": r.FormValue("alias"),
		})
	}

	alias, err := mail.ParseAlias(r.FormValue("alias"))
	if err != nil {
		refuse(http.StatusBadRequest, capitalise(err.Error())+".")
		return
	}

	switch err := s.store.RenameAccount(r.Context(), me.ID, id, alias); {
	case errors.Is(err, mail.ErrNotFound):
		// Same wording as unlink, for the same reason: confirming an id is real but not
		// yours is itself a disclosure.
		http.Error(w, "no such mailbox", http.StatusNotFound)
		return
	case err != nil:
		refuse(http.StatusConflict, capitalise(err.Error())+".")
		return
	}
	http.Redirect(w, r, "/accounts?renamed="+url.QueryEscape(alias), http.StatusSeeOther)
}

// capitalise upper-cases the first letter of an error so it can be shown as a sentence.
// Errors are written lower-case to read well when wrapped; the UI shows them on their own.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	id := mail.AccountID(r.FormValue("id"))
	if err := s.store.UnlinkAccount(r.Context(), me.ID, id); err != nil {
		// Reported the same way whether the mailbox never existed or belongs to somebody
		// else: confirming that an id is real but not yours is itself a disclosure.
		http.Error(w, "no such mailbox", http.StatusNotFound)
		return
	}
	s.providers.Forget(id)
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// --- Grants ---

// grantView is one grant as the pages draw it.
//
// The capabilities arrive split rather than as one list, because the question the grants page
// exists to answer at a glance is which grants hold something that matters. A privileged
// capability in the middle of `read,attachments,send,labels,settings,destructive` is not
// findable, and colouring it would only make it findable to somebody who can see the colour —
// so the privileged ones are listed under a heading of their own, in words.
type grantView struct {
	ID    string
	Label string
	// ShortID is the tail of the id, which is the only handle on a grant that is guaranteed
	// to be its own. A label is whatever the client sent and duplicates freely: an operator
	// with two grants called `Claude` edited the one their client was not holding, watched
	// nothing happen, and had nothing on the page to tell the two apart.
	ShortID string
	// Ambiguous is set where another grant on the page carries the same label. The id is
	// shown on those and not on the rest, because a hex tail beside every name is noise
	// until the moment it is the only thing that distinguishes two of them.
	Ambiguous bool
	// MostRecent marks the in-force grant used most recently, which is the best available
	// answer to "which of these is the one my client is actually holding". Set only where
	// there is more than one in force, since it distinguishes nothing from itself.
	MostRecent bool

	Accounts   []string
	Caps       []capView
	Privileged []capView
	// Mode is how much this grant may do on its own. It is on the card beside what the grant
	// may do at all, because those are the two halves of the same question and an operator
	// looking at a client that has just surprised them needs both.
	Mode      modeView
	CreatedAt time.Time

	// LastUsed is the exact instant and LastUsedAgo how long ago that was. Both, because
	// "18 Aug 06:08" answers a different question from "2 days ago" and this page is read
	// for both: one finds the call in the audit log, the other says whether anything is
	// still using the grant at all.
	LastUsed    string
	LastUsedAgo string
	Idle        bool

	ExpiresIn   string
	ExpiresWhen string
	ExpiresSoon bool

	Revoked bool
	Expired bool
}

// soon is how close an expiry has to be before the page stops stating a date and starts
// saying how long is left. A fortnight is about the horizon over which somebody would still
// want to do something about it rather than find out when a client stops on a Monday.
const soon = 14 * 24 * time.Hour

// idle is how long a grant goes unused before the page says so out loud. Long enough that a
// monthly job is not reported as abandoned, short enough that a forgotten agent is.
const idle = 90 * 24 * time.Hour

func (s *Server) grants(w http.ResponseWriter, r *http.Request) {
	views, err := s.grantViews(r, "")
	if err != nil {
		http.Error(w, "could not load grants", http.StatusInternalServerError)
		return
	}
	// The bands orderGrants sorted into, handed to the template separately rather than as one
	// list it has to look for the boundaries in. A reader should be able to see where "still
	// has access" ends without reading the badge on every card.
	var live, lapsed, dead []grantView
	for _, v := range views {
		switch {
		case v.Revoked:
			dead = append(dead, v)
		case v.Expired:
			lapsed = append(lapsed, v)
		default:
			live = append(live, v)
		}
	}
	// A label is client-supplied and nothing makes it unique. Marking the ones that collide
	// is what lets the page show an id where an id is the only thing that would help, and
	// say plainly that the name is not the identity — which is the fact the operator who hit
	// this did not have.
	seen := map[string]int{}
	for _, v := range views {
		seen[v.Label]++
	}
	shared := 0
	mark := func(band []grantView) {
		for i := range band {
			if seen[band[i].Label] > 1 {
				band[i].Ambiguous = true
				shared++
			}
		}
	}
	mark(live)
	mark(lapsed)
	mark(dead)

	// orderGrants has already put the most recently used first within its band, so the mark
	// goes on the first live grant anything has ever presented.
	if len(live) > 1 {
		for i := range live {
			if live[i].LastUsed != "" {
				live[i].MostRecent = true
				break
			}
		}
	}

	data := map[string]any{
		"Live": live, "Lapsed": lapsed, "Revoked": dead,
		"SharedLabels": shared,
	}
	// Matched against the grants actually there, so the confirmation names something real
	// rather than repeating whatever the query string said.
	for _, v := range views {
		if v.Label == r.URL.Query().Get("edited") {
			data["Edited"] = v.Label
		}
	}
	// A removal says so, and reopens the disclosure it happened inside. A redirect draws the
	// page fresh, so the band an operator was working in would otherwise close under them
	// between the first removal and the second — and with the last one gone there is no band
	// left to say anything at all, which is the case the line above the list has to cover.
	if n, err := strconv.Atoi(r.URL.Query().Get("removed")); err == nil && n > 0 {
		data["Removed"] = n
		data["RevokedOpen"] = true
	}
	s.render(w, r, "grants", "Grants", "grants", data)
}

// grantViews draws the signed-in user's grants, or the one of them with the given id.
// Filtering after the owner-scoped query rather than fetching by id alone is what keeps a
// grant belonging to somebody else out of reach.
func (s *Server) grantViews(r *http.Request, only grant.ID) ([]grantView, error) {
	me, _ := currentUser(r)
	grants, err := s.store.ListGrants(r.Context(), me.ID)
	if err != nil {
		return nil, err
	}
	orderGrants(grants, time.Now())

	views := make([]grantView, 0, len(grants))
	for _, g := range grants {
		if only != "" && g.ID != only {
			continue
		}
		views = append(views, s.viewGrant(r, g))
	}
	return views, nil
}

// Bands, in the order the grants page puts them in. Which band a grant is in is the first
// thing its ordering depends on, because the question the page answers first is what has
// access — and a grant that has none cannot be the answer, however recently it was made.
const (
	// bandInForce: a token presenting this right now gets through.
	bandInForce = iota
	// bandExpired: not revoked, and not usable either. Its own band rather than either of
	// the neighbouring ones, because it is genuinely a third thing: filed with the live
	// grants it would claim access it does not have, and filed with the revoked ones it
	// would look finished when it is one edit — a new expiry — away from working again.
	bandExpired
	// bandRevoked: over, and cannot be brought back by anybody on this side.
	bandRevoked
)

// shortGrantID is the tail of an identifier, which is its random half: ids are a timestamp
// followed by ten bytes of randomness, so the end is what differs between two grants approved
// in the same session and the beginning is what they have in common.
func shortGrantID(id grant.ID) string {
	s := string(id)
	if _, rest, ok := strings.Cut(s, "_"); ok {
		s = rest
	}
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return s
}

func bandOf(g *grant.Grant, now time.Time) int {
	switch {
	case g.Revoked():
		return bandRevoked
	case g.Expired(now):
		return bandExpired
	default:
		return bandInForce
	}
}

// orderGrants sorts the grants page.
//
// Band first: whatever else is true of a revoked grant, it is not the one currently reaching
// somebody's mail, and letting one sit above a live grant inverts what the page is for.
//
// Then most recently used first. Now that last_used_at is actually written, it is the best
// available answer to "which of these is doing something", and somebody who has opened this
// page because an agent is misbehaving is looking at the grants that have been used, in the
// order they were used.
//
// Never-used grants sink to the bottom of their band rather than rising to the top of it.
// They are worth noticing — the page badges them for exactly that reason — but they cannot be
// the grant that just did something surprising, and a grant approved a minute ago and not yet
// presented would otherwise outrank the one that has been running all morning. Among them,
// newest first, which is the order they were approved in.
func orderGrants(grants []*grant.Grant, now time.Time) {
	slices.SortStableFunc(grants, func(a, b *grant.Grant) int {
		if d := cmp.Compare(bandOf(a, now), bandOf(b, now)); d != 0 {
			return d
		}
		switch au, bu := a.LastUsedAt, b.LastUsedAt; {
		case au != nil && bu != nil:
			if d := bu.Compare(*au); d != 0 {
				return d
			}
		case au != nil:
			return -1
		case bu != nil:
			return 1
		}
		if d := b.CreatedAt.Compare(a.CreatedAt); d != 0 {
			return d
		}
		// Two grants approved in the same second still have to come out in one order every
		// time, or the page reshuffles under a reader between one refresh and the next.
		return cmp.Compare(a.ID, b.ID)
	})
}

// viewGrant draws one grant for display. The mailbox lookups are scoped to the signed-in
// user, so a grant naming a mailbox that is not theirs renders as unlinked rather than
// resolving to somebody else's alias.
func (s *Server) viewGrant(r *http.Request, g *grant.Grant) grantView {
	me, _ := currentUser(r)
	now := time.Now()

	v := grantView{
		ID: string(g.ID), Label: g.Label, ShortID: shortGrantID(g.ID),
		Mode:      modeViewOf(g.Mode),
		CreatedAt: g.CreatedAt, Revoked: g.Revoked(), Expired: g.Expired(now),
	}
	for _, id := range g.Accounts {
		if acct, err := s.store.Account(r.Context(), me.ID, id); err == nil {
			v.Accounts = append(v.Accounts, acct.Alias)
		} else {
			// A grant naming a deleted mailbox should say so rather than showing a raw id.
			v.Accounts = append(v.Accounts, "(unlinked)")
		}
	}
	for _, c := range g.Caps.Slice() {
		if view := capViewOf(c); view.Privileged {
			v.Privileged = append(v.Privileged, view)
		} else {
			v.Caps = append(v.Caps, view)
		}
	}
	if g.LastUsedAt != nil {
		v.LastUsed = g.LastUsedAt.Format("2 Jan 15:04")
		v.LastUsedAgo = humanSince(*g.LastUsedAt, now)
		v.Idle = now.Sub(*g.LastUsedAt) > idle
	}
	if g.ExpiresAt != nil && !v.Expired {
		v.ExpiresIn = "expires " + g.ExpiresAt.Format("2 Jan 2006")
		v.ExpiresSoon = g.ExpiresAt.Sub(now) < soon
		v.ExpiresWhen = "expires " + humanUntil(*g.ExpiresAt, now)
	}
	return v
}

// humanSince and humanUntil say how far away an instant is, at the granularity somebody
// reading a page actually decides on. A date tells you when a grant expires; only the
// distance tells you whether that is a problem, and working it out from the date is the
// arithmetic this page exists to save.
func humanSince(t, now time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return spanOf(d) + " ago"
}

func humanUntil(t, now time.Time) string {
	d := t.Sub(now)
	if d < time.Hour {
		return "within the hour"
	}
	return "in " + spanOf(d)
}

func spanOf(d time.Duration) string {
	switch hours := int(d.Hours()); {
	case hours < 1:
		return count(int(d.Minutes()), "minute")
	case hours < 24:
		return count(hours, "hour")
	case hours < 24*60:
		return count(hours/24, "day")
	case hours < 24*365:
		return count(hours/24/30, "month")
	default:
		return count(hours/24/365, "year")
	}
}

func count(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// ownedGrant loads one of the signed-in user's grants by id.
//
// It reads the owner-scoped list and picks from it rather than fetching by id alone, for the
// same reason grantViews does: the query, not a comparison written afterwards, is what keeps
// another user's grant out of reach. A grant that is not theirs is missing rather than
// forbidden, so guessing an id learns nothing.
func (s *Server) ownedGrant(r *http.Request, id grant.ID) (*grant.Grant, error) {
	me, _ := currentUser(r)
	grants, err := s.store.ListGrants(r.Context(), me.ID)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, grant.ErrNotFound
}

// revokeGrant asks before it acts, on a page of its own.
//
// The markup used to carry an onsubmit confirm() that the content-security policy has always
// blocked, so the only destructive action in the product that reads as guarded was in fact
// immediate. Forbidding script is worth more than the dialog was, and this is the same
// question asked in a way the policy allows. It is asked for revoking and not for unlinking
// because the two undo differently: a mailbox is relinked by the person looking at the page,
// while an authorisation is rebuilt by whoever runs the client at the other end.
func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	id := grant.ID(r.FormValue("id"))

	if r.FormValue("confirm") != "yes" {
		views, err := s.grantViews(r, id)
		if err != nil {
			http.Error(w, "could not load grants", http.StatusInternalServerError)
			return
		}
		if len(views) == 0 {
			http.Error(w, "no such grant", http.StatusNotFound)
			return
		}
		s.render(w, r, "revoke", "Revoke grant", "grants", map[string]any{"Grant": views[0]})
		return
	}

	if err := s.store.RevokeGrant(r.Context(), me.ID, id); err != nil {
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	// Drop the tokens too. The grant check alone would refuse them, but leaving live token
	// rows behind for a dead grant is a needless thing to reason about later.
	_ = s.store.RevokeTokensForGrant(r.Context(), me.ID, id)
	http.Redirect(w, r, "/grants", http.StatusSeeOther)
}

// removeGrant takes a revoked grant off the page, and asks nothing first.
//
// The distinction revoking and unlinking are drawn on is what undoing costs, and this is
// below both of them: a revoked grant reaches nothing, its tokens stopped resolving the
// moment it was revoked, and removing it ends no access because there is none left to end.
// What it does is take a dead record out of a list, which is what the operator opened the
// page to do — and the record is not destroyed either, since the audit log still names it.
// A confirmation page here would be ceremony over nothing, and ceremony that guards nothing
// is how the ceremony that guards something stops being read.
//
// The refusal is deliberately one answer for a grant that is not there, one that belongs to
// somebody else, and one that is still live: RemoveGrant's predicate covers all three, and
// separating them would confirm which.
func (s *Server) removeGrant(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	if err := s.store.RemoveGrant(r.Context(), me.ID, grant.ID(r.FormValue("id"))); err != nil {
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/grants?removed=1", http.StatusSeeOther)
}

// removeRevokedGrants clears the band in one press.
//
// It reaches exactly what pressing Remove on every card would, one grant at a time, so it is
// guarded the way the page guards unlinking rather than by a confirmation page: a tick beside
// the button it enables. That is the same reasoning the mailbox page gives — the tick is what
// keeps an action reached by accident from being an action performed by accident — and it
// matters more here, because unlike a card's own button this one acts on grants that may be
// scrolled off the screen.
func (s *Server) removeRevokedGrants(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	n, err := s.store.RemoveRevokedGrants(r.Context(), me.ID)
	if err != nil {
		http.Error(w, "could not remove those grants", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/grants?removed="+strconv.Itoa(n), http.StatusSeeOther)
}

// --- Editing a grant ---
//
// Until this existed a grant was fixed at the moment of consent, and the only thing that
// could be done to a wrong one was revoke it — which costs the client its token and needs
// whoever runs the client to authorise again from scratch. Editing in place is what makes a
// mistake correctable and a scope adjustable by the person whose mail it is.
//
// It is also the one place in the product where what an already-issued token may do changes
// without anyone at the other end agreeing to it, so the two directions are not treated as
// the same act. Taking something away needs no ceremony: it can only leave the token with
// less than it had, and the operator can put it back from the same page. Handing something
// over goes through a confirmation that itemises exactly what is being handed over, because
// the client's token starts using it on its next call and nobody at that end was asked.

// grantChange is what an edit would do to a grant, kept in the two halves that matter rather
// than as a single "changed" flag: which of them is non-empty is what decides whether the
// operator is asked to confirm.
type grantChange struct {
	AddedAccounts   []accountView
	RemovedAccounts []accountView
	AddedCaps       []capView
	RemovedCaps     []capView

	// Expiry reads as a sentence and is empty when the expiry is unchanged. ExpiryValue is
	// the same thing for the audit log, where a date and the word "never" are easier to scan
	// than prose.
	Expiry       string
	ExpiryValue  string
	ExpiryWidens bool

	// The mode, when it is changing. Loosening one hands the client more initiative than it
	// had, which is the same shape as adding a capability — nobody at the client's end agrees
	// to it and its token picks it up on the next call — so it is confirmed the same way.
	// Tightening one needs no ceremony, for the same reason narrowing a scope does not.
	ModeFrom    modeView
	ModeTo      modeView
	ModeChanged bool
	ModeLoosens bool

	// Irreversible names the capabilities being added whose effects outlive the capability.
	// Every other permission can be taken back and leaves nothing behind; a message that has
	// been sent stays sent, a message that has been deleted stays deleted, and a draft that
	// has been discarded stays discarded.
	Irreversible []string
}

func (c grantChange) Widens() bool {
	return len(c.AddedAccounts) > 0 || len(c.AddedCaps) > 0 || c.ExpiryWidens || c.ModeLoosens
}

func (c grantChange) Narrows() bool {
	return len(c.RemovedAccounts) > 0 || len(c.RemovedCaps) > 0 ||
		(c.Expiry != "" && !c.ExpiryWidens) || (c.ModeChanged && !c.ModeLoosens)
}

func (c grantChange) Empty() bool { return !c.Widens() && !c.Narrows() }

// ConfirmLabel is what the button on the confirmation page says.
//
// Naming the irreversible capabilities in the button is the whole of the extra ceremony they
// get, and it is deliberate that there is no second dialog behind it. A second question
// asked about the same submission is answered the same way as the first within a week of
// seeing it; a button that says "Grant send and destructive" is read every time, because it
// is the thing being pressed.
func (c grantChange) ConfirmLabel() string {
	switch {
	case len(c.Irreversible) > 0:
		return "Grant " + joinWords(c.Irreversible)
	case c.ModeLoosens && len(c.AddedAccounts) == 0 && len(c.AddedCaps) == 0:
		// Naming the destination in the button, for the same reason the irreversible
		// capabilities are named in it: "Widen this grant" is read once and "Let it send
		// without asking" is read every time, because it is the thing being pressed.
		return "Set it to " + c.ModeTo.Name
	default:
		return "Widen this grant"
	}
}

// IrreversibleWords names those capabilities as prose, for a page that has to talk about
// them rather than list them.
func (c grantChange) IrreversibleWords() string { return joinWords(c.Irreversible) }

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func capViewOf(c mail.Capability) capView {
	return capView{Name: string(c), Description: capDescriptions[c], Privileged: c.Privileged()}
}

// changeTo works out what turning g into the submitted selection would do.
//
// The submitted mailbox ids are drawn from the operator's own mailboxes where they match one
// and labelled plainly where they do not. Nothing here refuses anything: the store is where
// an id that is not the operator's is refused, and leaving that as the single check means
// there is one place to read to know it holds, rather than two that could drift apart.
func changeTo(g *grant.Grant, owned []mail.Account, accounts []mail.AccountID, caps mail.Set, mode grant.Mode, expires *time.Time) grantChange {
	byID := make(map[mail.AccountID]mail.Account, len(owned))
	for _, a := range owned {
		byID[a.ID] = a
	}
	view := func(id mail.AccountID) accountView {
		if a, ok := byID[id]; ok {
			return accountView{ID: a.ID, Alias: a.Alias, Address: a.Address}
		}
		return accountView{ID: id, Alias: "not one of your mailboxes"}
	}

	had := make(map[mail.AccountID]bool, len(g.Accounts))
	for _, id := range g.Accounts {
		had[id] = true
	}
	want := make(map[mail.AccountID]bool, len(accounts))
	for _, id := range accounts {
		want[id] = true
	}

	var c grantChange
	for _, id := range accounts {
		if !had[id] {
			c.AddedAccounts = append(c.AddedAccounts, view(id))
		}
	}
	for _, id := range g.Accounts {
		if !want[id] {
			c.RemovedAccounts = append(c.RemovedAccounts, view(id))
		}
	}
	for _, name := range mail.AllCapabilities {
		switch {
		case caps.Has(name) && !g.Caps.Has(name):
			c.AddedCaps = append(c.AddedCaps, capViewOf(name))
			// The test is whether taking the capability back afterwards reaches what it did,
			// not how alarming it looks: sent mail stays sent, deleted mail stays deleted,
			// and a discarded draft stays discarded. `discard` is ordinary on the consent
			// screen and named here for that reason — this is the page where an already
			// issued token gains something, which is where naming it is worth a click.
			if name == mail.CapSend || name == mail.CapDestructive || name == mail.CapDiscard {
				c.Irreversible = append(c.Irreversible, string(name))
			}
		case !caps.Has(name) && g.Caps.Has(name):
			c.RemovedCaps = append(c.RemovedCaps, capViewOf(name))
		}
	}

	// Compared after resolving both sides, so that setting an old grant explicitly to the
	// mode it already behaves as is not a change. It would otherwise read as a tightening
	// from nothing to `confirm`, and ask the operator to confirm a difference they cannot see.
	if g.Mode.Resolved() != mode.Resolved() {
		c.ModeChanged = true
		c.ModeFrom, c.ModeTo = modeViewOf(g.Mode), modeViewOf(mode)
		c.ModeLoosens = grant.Looser(g.Mode, mode)
	}

	// Each phrase reads on its own, because the expiry is shown apart from what the grant
	// gains and loses rather than filed under one of them: removing an expiry widens and
	// adding one narrows, which is the opposite way round from everything above it and reads
	// as a mistake when the two are mixed together.
	day := "2 Jan 2006"
	switch {
	case sameExpiry(expires, g.ExpiresAt):
	case expires == nil:
		c.Expiry, c.ExpiryValue, c.ExpiryWidens = "removed — the grant will no longer expire at all", "never", true
	case g.ExpiresAt == nil:
		c.Expiry, c.ExpiryValue = "set to "+expires.Format(day)+", where it had none", expires.Format("2006-01-02")
	case expires.After(*g.ExpiresAt):
		c.Expiry = "moved out to " + expires.Format(day) + ", from " + g.ExpiresAt.Format(day)
		c.ExpiryValue, c.ExpiryWidens = expires.Format("2006-01-02"), true
	default:
		c.Expiry, c.ExpiryValue = "brought forward to "+expires.Format(day)+
			", from "+g.ExpiresAt.Format(day), expires.Format("2006-01-02")
	}
	return c
}

// sameExpiry compares two expiries at the granularity the form offers them in.
//
// Expiry is chosen in whole days, so "90 days" picked against a grant that already expires
// in 90 days is not a change — without this it would read as a widening by however many
// seconds have passed since the grant was approved, and every such edit would ask the
// operator to confirm a difference they cannot see and did not make.
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UTC().Format("2006-01-02") == b.UTC().Format("2006-01-02")
}

// editGrantForm draws the edit page with the grant's current scope already ticked.
//
// Unlike the consent screen this starts from what the grant holds rather than from nothing.
// The two are answering different questions: consent asks what a stranger should be given,
// and an empty form is the honest starting point for that; this asks what to change about
// something already granted, and a form that arrived empty would make every edit a rewrite.
func (s *Server) editGrantForm(w http.ResponseWriter, r *http.Request) {
	g, err := s.ownedGrant(r, grant.ID(r.URL.Query().Get("id")))
	if err != nil {
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	s.renderGrantEdit(w, r, http.StatusOK, g, g.Accounts, g.Caps, g.Mode, "keep", "")
}

// renderGrantEdit draws the edit form, ticked according to a selection rather than to the
// stored grant, so that a refused submission comes back with what was typed rather than
// silently reverting to what is stored.
func (s *Server) renderGrantEdit(w http.ResponseWriter, r *http.Request, status int,
	g *grant.Grant, accounts []mail.AccountID, caps mail.Set, mode grant.Mode, expires, message string) {
	me, _ := currentUser(r)

	owned, err := s.store.ListAccounts(r.Context(), me.ID)
	if err != nil {
		http.Error(w, "could not load mailboxes", http.StatusInternalServerError)
		return
	}
	ticked := make(map[mail.AccountID]bool, len(accounts))
	for _, id := range accounts {
		ticked[id] = true
	}
	// What the grant holds now, which is not the same thing as what is ticked: a submission
	// that came back refused is ticked as it was typed. Marking the stored scope on the rows
	// is what turns a form showing a state into one showing a change — an untick beside the
	// mark is something being taken away, a tick without one is something being handed over.
	held := make(map[mail.AccountID]bool, len(g.Accounts))
	for _, id := range g.Accounts {
		held[id] = true
	}
	views := make([]accountView, 0, len(owned))
	for _, a := range owned {
		views = append(views, accountView{
			ID: a.ID, Alias: a.Alias, Address: a.Address,
			Checked: ticked[a.ID], Current: held[a.ID],
		})
	}

	capViews := make([]capView, 0, len(mail.AllCapabilities))
	for _, c := range mail.AllCapabilities {
		v := capViewOf(c)
		v.Checked = caps.Has(c)
		v.Current = g.Caps.Has(c)
		capViews = append(capViews, v)
	}

	now := "it never expires"
	switch {
	case g.ExpiresAt == nil:
	case g.Expired(time.Now()):
		now = "it expired on " + g.ExpiresAt.Format("2 Jan 2006")
	default:
		now = "it expires on " + g.ExpiresAt.Format("2 Jan 2006")
	}

	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "grant_edit", "Edit grant", "grants", map[string]any{
		"Grant":     s.viewGrant(r, g),
		"Accounts":  views,
		"Caps":      capViews,
		"Modes":     modeViews(mode, g.Mode.Resolved()),
		"Expires":   cmp.Or(expires, "keep"),
		"ExpiryNow": now,
		"Message":   message,
		// The same field carries a refusal and a remark — "a grant needs at least one
		// mailbox" and "nothing to change" arrive the same way. The status is what tells
		// them apart, so it decides which of the two the page draws.
		"Refused": status != http.StatusOK,
	})
}

// editGrant applies a change to a grant, asking first if the change hands anything over.
//
// The confirmation is a page rather than a dialog for the reason revoking is: the policy has
// no 'unsafe-inline', so a confirm() in an on* attribute here would be markup that never runs
// and a gate that only appears to be one. It is asked for widening and not for narrowing
// because the two undo differently — the same reasoning that has revoking ask and unlinking
// not. A capability taken away can be given back from this page by the person looking at it;
// a capability handed over cannot be un-handed, because the token may have used it before the
// page has finished loading.
func (s *Server) editGrant(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	id := grant.ID(r.FormValue("id"))

	g, err := s.ownedGrant(r, id)
	if err != nil {
		// Reported the same way whether the grant never existed or belongs to somebody else:
		// confirming that an id is real but not yours is itself a disclosure.
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	if g.Revoked() {
		http.Error(w, "this grant has been revoked, and that cannot be undone", http.StatusConflict)
		return
	}

	accounts := make([]mail.AccountID, 0, len(r.Form["accounts"]))
	for _, a := range r.Form["accounts"] {
		accounts = append(accounts, mail.AccountID(a))
	}
	// The same reading the consent screen does, so a capability the consent screen would
	// have refused cannot be spelled into a grant through this form instead.
	caps, err := mail.SetFromStrings(r.Form["capabilities"])
	if err != nil {
		s.renderGrantEdit(w, r, http.StatusBadRequest, g, accounts, g.Caps, g.Mode,
			r.FormValue("expires_days"), capitalise(err.Error())+".")
		return
	}
	// An absent field leaves the mode alone, the way an absent expiry does. The form this
	// page renders always posts one, so the empty case is a submission from somewhere else —
	// and "somewhere else" must not be able to reset a grant to the default by omission.
	//
	// A value that is present and unrecognised is refused rather than resolved. The same
	// reading the consent screen does, for the same reason: a form naming a mode this build
	// does not have has drifted from the server, and quietly landing on `confirm` would hide
	// that while leaving the operator believing they had set the one they picked.
	mode := g.Mode
	if chosen := r.FormValue("mode"); chosen != "" {
		mode, err = grant.ParseMode(chosen)
		if err != nil {
			s.renderGrantEdit(w, r, http.StatusBadRequest, g, accounts, caps, g.Mode,
				r.FormValue("expires_days"), capitalise(err.Error())+".")
			return
		}
	}

	refuse := func(status int, msg string) {
		s.renderGrantEdit(w, r, status, g, accounts, caps, mode, r.FormValue("expires_days"), msg)
	}
	switch {
	case len(accounts) == 0:
		refuse(http.StatusBadRequest, "A grant needs at least one mailbox. Revoke it instead if it should reach none.")
		return
	case caps.Len() == 0:
		refuse(http.StatusBadRequest, "A grant needs at least one capability. Revoke it instead if it should do nothing.")
		return
	}

	expires := g.ExpiresAt
	if choice := r.FormValue("expires_days"); choice != "" && choice != "keep" {
		parsed, err := grant.ParseExpiry(choice, time.Now())
		if err != nil {
			refuse(http.StatusBadRequest, capitalise(err.Error())+".")
			return
		}
		// Keep the stored instant when the choice lands on the day it already expires, so an
		// edit that leaves the expiry alone does not rewrite it by a handful of seconds.
		if !sameExpiry(parsed, g.ExpiresAt) {
			expires = parsed
		}
	}

	change := changeTo(g, s.ownedAccounts(r), accounts, caps, mode, expires)
	if change.Empty() {
		refuse(http.StatusOK, "Nothing to change — the grant already covers exactly that.")
		return
	}
	if change.Widens() && r.FormValue("confirm") != "yes" {
		s.render(w, r, "grant_widen", "Widen grant", "grants", map[string]any{
			"Grant":  s.viewGrant(r, g),
			"Change": change,
			// Resubmitted as hidden fields and re-read from scratch on the way back in.
			// Nothing here is trusted the second time either: ownership, the capability
			// names and the expiry are all checked again against the store.
			"Accounts":     accounts,
			"Capabilities": caps.Strings(),
			"Mode":         string(mode),
			"ExpiresDays":  r.FormValue("expires_days"),
		})
		return
	}

	// The store is the check that matters. It is scoped to the signed-in user and it refuses
	// a mailbox that is not theirs, so an id posted straight at this endpoint gets no further
	// than one chosen from the form above.
	if err := s.store.EditGrant(r.Context(), me.ID, id, accounts, caps, mode, expires); err != nil {
		if errors.Is(err, grant.ErrNotFound) {
			http.Error(w, "no such grant", http.StatusNotFound)
			return
		}
		refuse(http.StatusForbidden, capitalise(err.Error())+".")
		return
	}
	s.recordGrantEdit(r, g, change)

	s.log.Info("grant edited", "grant", id, "widened", change.Widens(),
		"mailboxes", len(accounts), "capabilities", caps.String(), "mode", string(mode))
	http.Redirect(w, r, "/grants?edited="+url.QueryEscape(g.Label), http.StatusSeeOther)
}

// ownedAccounts is the signed-in user's mailboxes, or none if they cannot be read. The
// callers use it to put names to ids for display, and a page that cannot name them is worth
// more than an error page.
func (s *Server) ownedAccounts(r *http.Request) []mail.Account {
	me, _ := currentUser(r)
	owned, err := s.store.ListAccounts(r.Context(), me.ID)
	if err != nil {
		return nil
	}
	return owned
}

// recordGrantEdit writes what the edit changed to the audit log.
//
// An edit to a live grant is exactly what that log is for: it is the only way the scope of an
// already-issued token moves, and reading the grant afterwards shows where it ended up
// without saying that it ever moved. One row per thing changed rather than one row per edit,
// because a mailbox row can carry the account id and be rendered as the alias it has now —
// aliases are mutable, and an audit row holding one would be a record that quietly rewords
// itself.
//
// Failures are logged rather than returned. The edit has already happened by this point, so
// refusing prevents nothing and would report a failure that did not occur; that is the same
// split the gate makes between a read it can still withhold and a change it cannot.
func (s *Server) recordGrantEdit(r *http.Request, g *grant.Grant, c grantChange) {
	entries := make([]grant.Audit, 0, 4)
	add := func(account mail.AccountID, outcome string, detail grant.Detail) {
		entries = append(entries, grant.Audit{
			OwnerID: g.OwnerID, GrantID: g.ID, AccountID: account,
			Tool: "grant.edit", Outcome: outcome, Detail: detail, At: time.Now(),
		})
	}
	// The mailbox rows name the mailbox in the detail as well as in the account column. The
	// column renders whatever the alias is now, which is the right thing for a live mailbox
	// and no help at all once one has been renamed or unlinked; the address is what it was
	// when the operator ticked the box.
	for _, a := range c.AddedAccounts {
		add(a.ID, "mailbox added", grant.Detail{Action: "add", Name: a.Address})
	}
	for _, a := range c.RemovedAccounts {
		add(a.ID, "mailbox removed", grant.Detail{Action: "remove", Name: a.Address})
	}
	if len(c.AddedCaps) > 0 || len(c.RemovedCaps) > 0 {
		var parts []string
		for _, v := range c.AddedCaps {
			parts = append(parts, "+"+v.Name)
		}
		for _, v := range c.RemovedCaps {
			parts = append(parts, "-"+v.Name)
		}
		add("", "capabilities "+strings.Join(parts, " "),
			grant.Detail{Action: strings.Join(parts, " ")})
	}
	if c.ExpiryValue != "" {
		add("", "expiry "+c.ExpiryValue, grant.Detail{Action: c.ExpiryValue})
	}
	if c.ModeChanged {
		// Written whichever way it moved. Tightening needs no confirmation and is still a
		// change to what an already-issued token may do on its own, which is exactly what
		// this log is for.
		add("", "mode "+c.ModeFrom.Name+" → "+c.ModeTo.Name,
			grant.Detail{Action: c.ModeFrom.Name + " → " + c.ModeTo.Name})
	}

	for _, e := range entries {
		if err := s.store.Record(r.Context(), e); err != nil {
			s.log.Warn("could not record a grant edit in the audit log",
				"grant", g.ID, "outcome", e.Outcome, "err", err)
		}
	}
}

// auditWindow is how far back the page reads. Everything on it — the day headings, the
// refusal count, the filter — describes this window and says so, because a count that
// silently covers "some of it" is worse than no count.
const auditWindow = 200

// auditRow is one logged call as the table draws it.
//
// Refused and Changed are separated because they read alike and mean opposite things. A row
// whose outcome is `scope_denied` is the gate turning a client away; a row whose outcome is
// "mailbox added" is an edit the operator themselves made, written to the same column. The
// table before this drew both in the colour it used for trouble.
//
// Everything from Capability down is disclosed rather than drawn on the line. The reason
// anybody reads this page is to find the one unusual row among hundreds of ordinary ones, and
// a table wide enough to show every fact about every call is a table nobody can scan. So the
// five columns stay exactly as they were and each row opens.
type auditRow struct {
	Time    string
	Grant   string
	Account string
	Tool    string
	Outcome string
	Refused bool
	Changed bool
	// Held is a privileged call a grant's mode stopped: recorded, not performed, and waiting
	// for its owner on the Held page. Neither a refusal nor a change.
	Held bool

	// Capability is empty for a call that needed none, which the page states in words. A row
	// with nothing recorded at all is Undetailed instead, and says so.
	Capability string
	// Affected is worded here rather than in the template, because the noun depends on the
	// tool: twelve results, twelve messages and twelve recipients are all "12" in the column
	// and three different facts.
	Affected string
	Targets  []string
	More     int
	Action   string
	Name     string
	To       []string
	Cc       []string
	Bcc      []string
	Subject  string
	Reason   string
	// FromDraft marks a send whose recipients this call never saw, so the page can say where
	// they were set instead of showing a send that appears to have gone to nobody.
	FromDraft bool
	// Undetailed marks a row written before any of the above was recorded. Rendering it as an
	// ordinary row with every fact empty would be a claim about the call; this is a claim
	// about the log, which is the true one.
	Undetailed bool
}

type auditDay struct {
	Label string
	Rows  []auditRow
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	entries, err := s.store.RecentAudit(r.Context(), me.ID, auditWindow)
	if err != nil {
		http.Error(w, "could not load the audit log", http.StatusInternalServerError)
		return
	}

	// The one thing somebody reads an audit log for is the call that did not go through, and
	// finding it meant reading every row. This is that filter, done the only way a page with
	// no script can do it: a link back to the same handler.
	onlyRefused := r.URL.Query().Get("show") == "refused"

	now := time.Now()
	var days []auditDay
	var refusals int
	for _, e := range entries {
		row := auditRow{
			Time: e.At.Format("15:04:05"), Grant: e.GrantName, Account: e.Account,
			Tool: e.Tool, Outcome: e.Outcome,
			Changed: e.Tool == "grant.edit",

			Capability: e.Capability,
			Affected:   affectedLabel(e.Tool, e.Affected),
			Targets:    e.Detail.IDs,
			More:       e.Detail.More,
			Action:     e.Detail.Action,
			Name:       e.Detail.Name,
			To:         e.Detail.To,
			Cc:         e.Detail.Cc,
			Bcc:        e.Detail.Bcc,
			Subject:    e.Detail.Subject,
			Reason:     e.Reason,
			FromDraft:  e.Tool == "mail.send" && e.Detail.Action == "draft",
			Undetailed: !e.Detailed,
		}
		// `held` is a third thing, and counting it as a refusal would be wrong in both
		// directions: nothing was turned away, and nothing was done either. It belongs to a
		// grant in `hold` mode and its answer is on the Held page, not here.
		row.Held = e.Outcome == "held"
		row.Refused = e.Outcome != "ok" && !row.Changed && !row.Held
		if row.Refused {
			refusals++
		}
		if onlyRefused && !row.Refused {
			continue
		}
		label := dayLabel(e.At, now)
		if len(days) == 0 || days[len(days)-1].Label != label {
			days = append(days, auditDay{Label: label})
		}
		days[len(days)-1].Rows = append(days[len(days)-1].Rows, row)
	}

	s.render(w, r, "audit", "Audit", "audit", map[string]any{
		"Days":        days,
		"Refusals":    refusals,
		"Total":       len(entries),
		"Window":      auditWindow,
		"OnlyRefused": onlyRefused,
	})
}

// affectedLabel puts a noun on a count.
//
// The number alone is ambiguous in the one place it matters most: "mail.send 3" is three
// recipients, not three messages, and an operator reading it as three messages would go
// looking for two sends that never happened. Anything unrecognised is "items", which is vague
// and not wrong — a tool added later reads awkwardly here rather than reading as a lie.
func affectedLabel(tool string, n *int) string {
	if n == nil {
		return ""
	}
	noun := "items"
	switch tool {
	case "mail.search":
		noun = "results"
	case "mail.send":
		noun = "recipients"
	case "mail.labels":
		noun = "labels"
	case "mail.filters":
		noun = "filters"
	case "mail.settings":
		noun = "entries"
	case "mail.get_message", "mail.get_thread", "mail.modify", "mail.trash", "mail.draft":
		noun = "messages"
	case "mail.get_attachment":
		noun = "attachments"
	}
	if *n == 1 {
		noun = strings.TrimSuffix(noun, "s")
	}
	return fmt.Sprintf("%d %s", *n, noun)
}

// dayLabel names the day a row belongs to. Today and yesterday by those names, because that
// is what somebody scanning for what an agent did this morning is looking for.
func dayLabel(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	sameDay := func(a, b time.Time) bool {
		ay, am, ad := a.Date()
		by, bm, bd := b.Date()
		return ay == by && am == bm && ad == bd
	}
	switch {
	case sameDay(at, now):
		return "Today"
	case sameDay(at, now.AddDate(0, 0, -1)):
		return "Yesterday"
	default:
		return at.Format("Monday 2 January 2006")
	}
}

// --- Consent ---

type capView struct {
	Name        string
	Description string
	Privileged  bool
	Checked     bool
	// Current is what the grant holds now, on a form where Checked is what the operator
	// has asked for. They are the same on a first render and differ on the way back from a
	// refusal; the edit page marks the difference so an edit reads as a change rather than
	// as a state. The consent screen leaves it false — nothing is held there yet.
	Current bool
	// Requested marks a capability the client named in its scope. It is never Checked on
	// account of it: what was asked for and what is being granted are two different things,
	// and the consent screen shows them as two different things — a mark beside the name,
	// on a box that is still empty until somebody ticks it.
	Requested bool
}

// modeView is one of the three modes as a form draws it. The wording comes from
// grant.Mode itself rather than from a table here, so the consent screen, the edit page and
// the widen page cannot describe the same mode three ways.
type modeView struct {
	Name    string
	Title   string
	Summary string
	// Brief completes the sentence the consent screen's running summary builds. It is
	// rendered into the markup and read back by the script, so that the line a script puts on
	// that page is a line this server wrote.
	Brief string
	// Enforced is the fact the whole feature turns on, and it is on the page beside every
	// option rather than in a paragraph underneath: two of these three modes are wording sent
	// to a client, and one of them is something this server refuses to do. An operator
	// choosing between them has to be able to see which is which.
	Enforced bool
	Checked  bool
	// Current is the same distinction capView draws: what the grant is set to now, against
	// what the form is asking for.
	Current bool
}

func modeViewOf(m grant.Mode) modeView {
	m = m.Resolved()
	return modeView{
		Name: string(m), Title: m.Title(), Summary: m.Summary(), Brief: m.Brief(),
		Enforced: m.Enforced(),
	}
}

// modeViews is the three of them, ticked according to a selection and marked according to
// what the grant is set to.
//
// `current` is compared literally rather than resolved, so an empty one marks nothing. That
// is the consent screen, where no mode is current because there is no grant yet; the edit
// page passes the grant's resolved mode, which marks `confirm` on a grant that predates modes
// — correctly, because that is what such a grant does.
func modeViews(checked, current grant.Mode) []modeView {
	out := make([]modeView, 0, len(grant.AllModes))
	for _, m := range grant.AllModes {
		v := modeViewOf(m)
		v.Checked = m == checked.Resolved()
		v.Current = m == current
		out = append(out, v)
	}
	return out
}

// accountView is a mailbox as the consent form draws it. Whether a box is ticked is a
// property of this rendering rather than of the mailbox, so it is decided here rather than
// being threaded through mail.Account.
type accountView struct {
	ID      mail.AccountID
	Alias   string
	Address string
	Checked bool
	// Current is the same distinction capView draws: what the grant reaches now, against
	// what the form is asking for.
	Current bool
}

var capDescriptions = map[mail.Capability]string{
	mail.CapRead:        "Search and read mail, list labels, see attachment names",
	mail.CapAttachments: "Download attachment contents",
	mail.CapDraft:       "Create and edit drafts — but not send or delete them",
	mail.CapDiscard:     "Delete drafts, including ones you wrote yourself",
	mail.CapSend:        "Send mail, reply and forward",
	mail.CapLabels:      "Apply labels, archive, star, mark read, create labels — not the bin, junk, or deleting a folder",
	mail.CapFilters:     "Create and delete filters and rules — not ones that forward mail elsewhere",
	mail.CapSettings:    "Aliases, vacation responder, forwarding, delegation",
	mail.CapDestructive: "Trash and permanently delete messages, not drafts. Also needed to move mail to the bin or to junk by labelling it, which is the same act",
}

// ConsentPage renders the approval form. Nothing is preselected, including whatever the
// client asked for: a consent screen with the boxes already ticked in the requester's favour
// is a formality rather than a decision.
//
// A box is ticked only where the request says the operator ticked it, which happens when the
// screen comes back round from a select-all. The selections are empty on a first render, so
// that path is unchanged.
func (s *Server) ConsentPage(w http.ResponseWriter, r *http.Request, req oauthsrv.ConsentRequest) {
	tickedCaps := make(map[mail.Capability]bool, len(req.SelectedCaps))
	for _, c := range req.SelectedCaps {
		tickedCaps[c] = true
	}
	// What the client asked for, shown and marked but never ticked.
	//
	// Read through ParseCapability rather than printed as it arrived. Registration is open,
	// so the scope is attacker-controlled text on the page whose job is helping a human
	// decide, and a scope of two hundred invented words would otherwise be two hundred words
	// of client-supplied prose above the buttons. What is left is a list this build has
	// names for; that anything else was asked for is worth saying, but only as a sentence.
	requested := make(map[mail.Capability]bool, len(req.RequestedCaps))
	unrecognised := false
	for _, name := range req.RequestedCaps {
		c, err := mail.ParseCapability(name)
		if err != nil {
			unrecognised = true
			continue
		}
		requested[c] = true
	}
	requestedNames := make([]string, 0, len(requested))
	for _, c := range mail.AllCapabilities {
		if requested[c] {
			requestedNames = append(requestedNames, string(c))
		}
	}
	caps := make([]capView, 0, len(req.Capabilities))
	for _, c := range req.Capabilities {
		caps = append(caps, capView{
			Name:        string(c),
			Description: capDescriptions[c],
			Privileged:  c.Privileged(),
			Checked:     tickedCaps[c],
			Requested:   requested[c],
		})
	}

	tickedAccounts := make(map[mail.AccountID]bool, len(req.SelectedAccounts))
	for _, id := range req.SelectedAccounts {
		tickedAccounts[id] = true
	}
	accounts := make([]accountView, 0, len(req.Accounts))
	for _, a := range req.Accounts {
		accounts = append(accounts, accountView{
			ID: a.ID, Alias: a.Alias, Address: a.Address, Checked: tickedAccounts[a.ID],
		})
	}

	// The expiry default lives here rather than as a `selected` attribute in the markup. In
	// the markup it would win back every time the page came round from a select-all, quietly
	// undoing an expiry the operator had already changed.
	s.render(w, r, "consent", "Authorize", "", map[string]any{
		"Req":  req,
		"Caps": caps,
		// The three modes, with the default ticked. This is the one group on this screen
		// that arrives with something selected, and it has to: a grant has a mode whether or
		// not anybody picks one, so an empty radiogroup would misdescribe what Approve does.
		// Nothing is granted by it either way — a mode decides how a capability is exercised,
		// never whether the client holds it.
		"Modes":            modeViews(req.Mode, ""),
		"Accounts":         accounts,
		"Requested":        requestedNames,
		"RequestedUnknown": unrecognised,
		"Expires":          cmp.Or(req.ExpiresDays, "90"),
	})
}

// --- Login ---

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", "Sign in", "", map[string]any{
		"Methods": s.operator.Methods(),
		"Next":    r.URL.Query().Get("next"),
		"Error":   "",
	})
}
