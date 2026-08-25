package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// Method is one configured way to sign in, as the login page needs to render it.
type Method struct {
	ID    string
	Label string
	Kind  string // oidc
}

// Registry lets several identity providers serve one instance at once.
//
// A deployment usually wants more than one: staff signing in through the company issuer, and
// a second issuer for contractors or for the operator's own account. Each provider maps to
// its own user, keyed on (issuer, subject), so the same human arriving through two of them is
// two users rather than one silently merged identity — merging is a decision with somebody's
// mail on the other side of it, and belongs to a person rather than to a heuristic.
//
// There is deliberately no password provider. A password on a service whose entire job is
// holding other people's mail credentials is the weakest link in it: no revocation, no device
// or session policy, no audit trail beyond this process, and one leaked string away from
// every linked mailbox. Every deployment already has an identity provider it trusts more.
type Registry struct {
	oidcs    []*OIDC
	forward  *Forward
	sessions *Sessions
}

func NewRegistry(sessions *Sessions) *Registry {
	return &Registry{sessions: sessions}
}

func (r *Registry) AddOIDC(o *OIDC)       { r.oidcs = append(r.oidcs, o) }
func (r *Registry) SetForward(f *Forward) { r.forward = f }

func (r *Registry) OIDCs() []*OIDC    { return r.oidcs }
func (r *Registry) Forward() *Forward { return r.forward }

// OIDCByID finds the provider a callback belongs to.
func (r *Registry) OIDCByID(id string) (*OIDC, bool) {
	for _, o := range r.oidcs {
		if o.ID() == id {
			return o, true
		}
	}
	return nil, false
}

// Methods lists the interactive sign-in choices, in configuration order. Forward-auth is
// absent on purpose: it has no button, since the proxy has already decided.
func (r *Registry) Methods() []Method {
	var out []Method
	for _, o := range r.oidcs {
		out = append(out, Method{ID: o.ID(), Label: o.Label(), Kind: "oidc"})
	}
	return out
}

func (r *Registry) Mode() string {
	var parts []string
	for _, o := range r.oidcs {
		parts = append(parts, "oidc:"+o.ID())
	}
	if r.forward != nil {
		parts = append(parts, "forward")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// Identify resolves the caller.
//
// An explicit session wins over a forwarded header: somebody who signed in interactively
// stays who they signed in as, even on an instance that also sits behind an authenticating
// proxy.
func (r *Registry) Identify(req *http.Request) (Operator, error) {
	if c, err := req.Cookie(sessionCookie); err == nil {
		if op, ok := r.sessions.Get(c.Value); ok {
			return op, nil
		}
	}
	if r.forward != nil {
		return r.forward.Identify(req)
	}
	return Operator{}, ErrNoSession
}

// Authorize routes the decision to whichever provider issued this identity.
//
// Falling through to "allow" would be the wrong default: a session whose issuer matches
// nothing configured is one whose provider has been removed, and it should stop working
// rather than keep its access on the strength of a cookie.
func (r *Registry) Authorize(op Operator) error {
	switch op.Issuer {
	case "":
		return ErrNotAuthorized
	case "local":
		// Sessions and user rows from the removed password provider. Refusing rather than
		// ignoring matters: the row still owns mailboxes, and it is reached now by adopting
		// it with `mailroom invite --adopt-owner`, not by keeping an old cookie alive.
		return ErrNotAuthorized
	case "forward-auth":
		if r.forward == nil {
			return ErrNotAuthorized
		}
		return r.forward.Authorize(op)
	}

	for _, o := range r.oidcs {
		if o.Matches(op.Issuer) {
			return o.Authorize(op)
		}
	}
	return ErrNotAuthorized
}

// StartLogin sends the caller somewhere they can sign in.
//
// With exactly one interactive method there is nothing to choose, so it goes straight there;
// a chooser listing a single button is a page nobody needs. With several, or with none, the
// login page explains the situation.
func (r *Registry) StartLogin(w http.ResponseWriter, req *http.Request) bool {
	methods := r.Methods()
	if len(methods) == 1 && methods[0].Kind == "oidc" {
		if o, ok := r.OIDCByID(methods[0].ID); ok {
			return o.StartLogin(w, req)
		}
	}
	if len(methods) == 0 {
		// Forward-auth only: there is no interactive login to offer, and rendering one would
		// be a path that could never succeed.
		return false
	}

	next := req.URL.Path
	http.Redirect(w, req, "/login?next="+next, http.StatusSeeOther)
	return true
}

func (r *Registry) Logout(w http.ResponseWriter, req *http.Request) {
	if c, err := req.Cookie(sessionCookie); err == nil {
		r.sessions.Delete(c.Value)
	}
	clearSessionCookie(w, r.secureCookies())
}

func (r *Registry) secureCookies() bool {
	for _, o := range r.oidcs {
		return o.secure
	}
	return true
}

// Validate reports a configuration that cannot authenticate anybody.
func (r *Registry) Validate() error {
	if len(r.oidcs) == 0 && r.forward == nil {
		return fmt.Errorf("no login method is configured")
	}
	seen := map[string]bool{}
	for _, o := range r.oidcs {
		if seen[o.ID()] {
			return fmt.Errorf("two identity providers share the id %q; ids appear in callback URLs and must be unique", o.ID())
		}
		seen[o.ID()] = true
	}
	return nil
}

var _ Provider = (*Registry)(nil)
