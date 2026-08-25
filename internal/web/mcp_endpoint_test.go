package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// renderAs draws one page the way a signed-in browser would, without the middleware.
func renderAs(t *testing.T, page http.HandlerFunc, path string, who user.User) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r = r.WithContext(user.NewContext(r.Context(), who))
	rec := httptest.NewRecorder()
	page(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: want 200, got %d: %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The endpoint is the one thing about an instance a person setting up an MCP client cannot
// work out for themselves, and it used to be passed on out of band. It has to be on the
// pages somebody setting a client up is already looking at: the empty first run most of
// all, and still there once mailboxes and grants exist.
func TestBothPagesShowTheMCPEndpoint(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	want := s.publicURL + "/mcp"

	signInAs(s, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	// First run: no mailboxes and no grants, which is exactly when a client is being set up.
	for _, page := range []struct {
		path   string
		render http.HandlerFunc
	}{
		{"/accounts", s.accounts},
		{"/grants", s.grants},
	} {
		body := renderAs(t, page.render, page.path, me)
		if !strings.Contains(body, want) {
			t.Errorf("%s on a first run does not show %s: %s", page.path, want, body)
		}
	}

	// And once there is something to look at, so it is not only a first-run hint.
	me, _ = aGrant(t, s, db)
	for _, page := range []struct {
		path   string
		render http.HandlerFunc
	}{
		{"/accounts", s.accounts},
		{"/grants", s.grants},
	} {
		body := renderAs(t, page.render, page.path, me)
		if !strings.Contains(body, want) {
			t.Errorf("%s with a mailbox and a grant does not show %s: %s", page.path, want, body)
		}
	}
}

// Copied out of a field that can be typed into, the value pasted into a client is not
// necessarily the value rendered. The field is also the only thing on the page carrying a
// URL nobody can read out of a paragraph, so it needs a label of its own.
func TestTheMCPEndpointIsReadonlyAndLabelled(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	body := renderAs(t, s.grants, "/grants", me)
	at := strings.Index(body, `id="mcp-endpoint"`)
	if at < 0 {
		t.Fatalf("no endpoint field on the page at all: %s", body)
	}
	field := body[at:]
	field = field[:strings.Index(field, ">")+1]

	if !strings.Contains(field, "readonly") {
		t.Errorf("the endpoint field can be typed into: %s", field)
	}
	if !strings.Contains(body, `for="mcp-endpoint"`) {
		t.Errorf("the endpoint field has no label, so it is unnamed to a screen reader: %s", body)
	}
}

// A second instance is a different address, and this is the whole reason the value is
// rendered rather than documented.
func TestTheMCPEndpointFollowsTheConfiguredPublicURL(t *testing.T) {
	// With a trailing slash, which is trimmed: the endpoint is one path join away from
	// reading https://mail.elsewhere.example//mcp.
	s, db := testServerAt(t, signup.Policy{Mode: signup.Open}, &config.Config{},
		"https://mail.elsewhere.example/")
	me, _ := aGrant(t, s, db)

	body := renderAs(t, s.accounts, "/accounts", me)
	if !strings.Contains(body, "https://mail.elsewhere.example/mcp") {
		t.Errorf("the page does not show this instance's own endpoint: %s", body)
	}
	if strings.Contains(body, "mail.example.com") {
		t.Errorf("the page shows an endpoint from somewhere other than its configuration: %s", body)
	}
}
