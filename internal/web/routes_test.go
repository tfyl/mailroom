package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
)

// recordedRoute is one registration: the pattern exactly as written, and what it was
// registered with.
type recordedRoute struct {
	pattern string
	handler http.Handler
}

// method returns the verb the pattern names, or "" for a pattern that names none.
//
// A pattern with no method matches every method, POST included, so "" is a finding rather
// than a detail: it is the second way a write can end up outside the CSRF check, and it does
// not look like one in a list of routes.
func (r recordedRoute) method() string {
	verb, _, found := strings.Cut(r.pattern, " ")
	if !found {
		return ""
	}
	return verb
}

func (r recordedRoute) path() string {
	_, path, found := strings.Cut(r.pattern, " ")
	if !found {
		return r.pattern
	}
	return path
}

// recordingRouter keeps the route table instead of serving it, so a test can ask what was
// registered rather than read the source and hope.
type recordingRouter struct{ routes []recordedRoute }

func (rr *recordingRouter) Handle(pattern string, handler http.Handler) {
	rr.routes = append(rr.routes, recordedRoute{pattern: pattern, handler: handler})
}

func (rr *recordingRouter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	rr.Handle(pattern, http.HandlerFunc(handler))
}

// browserSurface registers the whole browser surface into a recorder, over a server whose
// session store this test holds.
//
// Holding the store is what lets signing out be checked by asking whether the session is
// still there, rather than by reading a Set-Cookie header and trusting it meant something.
// Forward-auth is configured because it is the one mode that makes a session authorizable
// without a live issuer to discover: Registry.Authorize routes a "forward-auth" session back
// to it, and every other issuer is refused when no OIDC provider is configured.
func browserSurface(t *testing.T) (*recordingRouter, *Server, *auth.Sessions, *http.Cookie) {
	t.Helper()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})

	sessions := auth.NewSessions(time.Hour)
	registry := auth.NewRegistry(sessions)
	forward, err := auth.NewForward("X-Forwarded-User", []string{"192.0.2.1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	registry.SetForward(forward)
	s.operator = registry

	token, err := sessions.Create(auth.Operator{
		Issuer:  "forward-auth",
		Subject: "ada@example.com",
		Email:   "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	rr := &recordingRouter{}
	s.Routes(rr, oauthsrv.New(db, "https://mail.example.com"))
	return rr, s, sessions, &http.Cookie{Name: "mailroom_session", Value: token}
}

// signedInPost builds a request the way a signed-in operator's browser would, carrying
// whatever token is passed — including none.
func signedInPost(path string, session *http.Cookie, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(session)
	return r
}

// The general form of the bug, rather than the one route it was found on.
//
// Every mutating route is enumerated from the table this package actually registers and made
// to prove, one request each, that it refuses a signed-in submission carrying no CSRF token.
// A POST added later without the check fails here on the day it is added; before this test,
// POST /logout sat outside the check for as long as it existed and reading the list was the
// only thing that would have caught it.
func TestEveryMutatingRouteRefusesARequestWithNoCSRFToken(t *testing.T) {
	rr, _, _, session := browserSurface(t)

	mutating := 0
	for _, route := range rr.routes {
		if route.method() == "" {
			t.Errorf("%q names no method, so it answers POST as well as GET and nothing checks CSRF on it", route.pattern)
			continue
		}
		if route.method() == http.MethodGet {
			continue
		}
		mutating++

		t.Run(route.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			route.handler.ServeHTTP(rec, signedInPost(route.path(), session, url.Values{}))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403: a signed-in POST with no CSRF token reached the handler", rec.Code)
			}
			// The status alone is not enough: withUser answers 403 too, for a signup it
			// refuses, and that would let an unprotected route pass this test.
			if !strings.Contains(rec.Body.String(), "this form expired or came from another page") {
				t.Fatalf("403 for some other reason than the CSRF check: %s", rec.Body.String())
			}
		})
	}

	// A recorder that collected nothing, or a Routes that stopped registering writes, would
	// otherwise pass this test by having nothing to check.
	if mutating < 18 {
		t.Fatalf("only %d mutating routes were registered; the sweep is not covering the surface", mutating)
	}
}

// POST /logout is the route the sweep above was written for. It was registered bare, so any
// page on the internet could end an operator's session with a form submission.
func TestLogoutRequiresACSRFToken(t *testing.T) {
	rr, s, sessions, session := browserSurface(t)

	logout := handlerFor(t, rr, "POST /logout")

	// The token this instance would have rendered into a sign-out form, and one that is
	// merely well-formed.
	valid := s.csrf.token(httptest.NewRecorder(), signedInPost("/logout", session, url.Values{}), false)
	if valid == "" {
		t.Fatal("no token could be minted for a session that exists")
	}

	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{name: "no token at all", form: url.Values{}},
		{name: "an empty token", form: url.Values{"csrf_token": {""}}},
		{name: "a token that is not this session's", form: url.Values{"csrf_token": {s.csrf.sign("some-other-seed")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			logout.ServeHTTP(rec, signedInPost("/logout", session, tc.form))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403", rec.Code)
			}
			if _, ok := sessions.Get(session.Value); !ok {
				t.Fatal("the session was ended by a request that was supposed to be refused")
			}
		})
	}

	// And the fix must not have made signing out impossible, which is the way a CSRF check
	// most often goes wrong.
	rec := httptest.NewRecorder()
	logout.ServeHTTP(rec, signedInPost("/logout", session, url.Values{"csrf_token": {valid}}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: a valid token must still sign the operator out: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want \"/\"", got)
	}
	if _, ok := sessions.Get(session.Value); ok {
		t.Fatal("the session outlived a sign-out that was accepted")
	}
}

func handlerFor(t *testing.T, rr *recordingRouter, pattern string) http.Handler {
	t.Helper()
	for _, route := range rr.routes {
		if route.pattern == pattern {
			return route.handler
		}
	}
	t.Fatalf("%q is not registered", pattern)
	return nil
}
