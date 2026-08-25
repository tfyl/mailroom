// Package auth authenticates the operator — the human administering this instance.
//
// This is entirely separate from the grants issued to MCP clients. Two OAuth relationships
// exist in mailroom and conflating them is the mistake that makes per-client scoping
// impossible: this package answers "who is the human", while a grant answers "what may this
// client do". Nothing here is ever consulted for an MCP tool call.
package auth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Operator is an authenticated human.
//
// Issuer and Subject together identify the person. Subject alone is not enough: it is only
// unique within an issuer, so an instance that later adds a second identity provider could
// otherwise hand one person's mailboxes to another.
type Operator struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
	Groups  []string
}

var (
	// ErrNoSession means nobody is logged in. Handlers turn this into a redirect to login
	// rather than a 403, since it is the ordinary state for a first visit.
	ErrNoSession = errors.New("not authenticated")

	// ErrNotAuthorized means the identity is known but not permitted here. Distinct from
	// ErrNoSession because logging in again will not help.
	ErrNotAuthorized = errors.New("not authorized to administer this instance")
)

// Provider authenticates operators. Identify and Authorize are deliberately separate:
// "authenticated by your identity provider" and "allowed to administer these mailboxes" are
// different questions, and treating them as one is how an instance ends up reachable by
// everyone who happens to have an account at the issuer.
type Provider interface {
	// Mode names the configured mode, for display.
	Mode() string

	// Identify returns the operator making this request.
	Identify(r *http.Request) (Operator, error)

	// Authorize decides whether that operator may administer this instance.
	Authorize(Operator) error

	// StartLogin begins an interactive login, if the mode has one. Modes that authenticate
	// upstream (forward-auth) report false and the handler renders an error instead of a
	// login form that could never succeed.
	StartLogin(w http.ResponseWriter, r *http.Request) bool

	// Logout ends the session where the mode owns one.
	Logout(w http.ResponseWriter, r *http.Request)
}

type contextKey struct{}

// WithOperator stores the authenticated operator on the request context.
func WithOperator(ctx context.Context, op Operator) context.Context {
	return context.WithValue(ctx, contextKey{}, op)
}

// FromContext returns the operator authenticated for this request.
func FromContext(ctx context.Context) (Operator, bool) {
	op, ok := ctx.Value(contextKey{}).(Operator)
	return op, ok
}

// Require wraps a handler so that only an authenticated, authorized operator reaches it.
//
// This guards the browser surface only. The MCP endpoint and the OAuth token endpoints are
// deliberately not behind it: a remote MCP client has no browser to complete an interactive
// login with, and its bearer token is the gate instead.
func Require(p Provider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, err := p.Identify(r)
		if err != nil {
			if errors.Is(err, ErrNoSession) {
				if p.StartLogin(w, r) {
					return
				}
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return
		}
		if err := p.Authorize(op); err != nil {
			// Deliberately terse: an authenticated but unauthorized visitor should not learn
			// which group or claim would have let them in.
			http.Error(w, "not authorized", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithOperator(r.Context(), op)))
	})
}

const sessionCookie = "mailroom_session"

func setSessionCookie(w http.ResponseWriter, token string, secure bool, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

// loginCookie carries the binder that ties a sign-in attempt to the browser that started it.
// Short-lived by construction: it is set when the redirect to the issuer goes out and cleared
// the moment the callback is handled, successfully or not.
const loginCookie = "mailroom_login"

func setLoginCookie(w http.ResponseWriter, binder string, secure bool, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCookie,
		Value:    binder,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func clearLoginCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
