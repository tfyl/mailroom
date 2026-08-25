package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mcp"
	"github.com/tfyl/mailroom/internal/signup"
)

// footerOf returns the offer at the bottom of a rendered page, so the assertions below are
// about the footer rather than about anything else on the page that happens to contain the
// same words.
func footerOf(t *testing.T, body string) string {
	t.Helper()

	at := strings.Index(body, "<footer")
	if at < 0 {
		t.Fatalf("the page has no footer, so there is nowhere the source offer could be: %s", body)
	}
	return body[at:]
}

// The offer section 13 of the AGPL asks for. It is worth a test rather than a convention
// because nothing in this product stops working when it goes missing: the pages render, the
// tools answer, and the only thing lost is the obligation the licence was chosen for. This is
// what makes taking it out a deliberate act rather than a tidy-up.
func TestTheOperatorInterfaceOffersItsSource(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	footer := footerOf(t, renderAccountsPage(t, s, me))

	if !strings.Contains(footer, sourceURL(mcp.Version)) {
		t.Errorf("the page offers no way to get the source: %s", footer)
	}
	// Source without a version is not the *corresponding* source of anything, and
	// corresponding source is the only kind section 13 asks for.
	if !strings.Contains(footer, ">"+mcp.Version+"<") {
		t.Errorf("the page does not say which version is running: %s", footer)
	}
	if !strings.Contains(footer, "Affero") {
		t.Errorf("the page does not say what the source is offered under: %s", footer)
	}
}

// Signing in is an interaction with this program over a network too, and the sign-in page is
// the only page somebody without an account can reach. It draws the same layout as every
// other page, so this pins that the offer is not quietly scoped to the pages behind the
// guard.
func TestTheSignInPageOffersItsSource(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Open})

	rec := httptest.NewRecorder()
	s.loginForm(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if footer := footerOf(t, rec.Body.String()); !strings.Contains(footer, sourceRepo) {
		t.Errorf("somebody who has not signed in is offered no source: %s", footer)
	}
}

// Where the offer points. The release workflow stamps either a tag or main-<commit>, and both
// have to resolve to the tree they name — a link to a branch that has moved on is source, but
// not the source of what is answering. A build that names no revision has to fall back to
// something that resolves at all, rather than to a plausible-looking 404.
func TestTheSourceLinkNamesTheRunningRevision(t *testing.T) {
	sha := "0f2c1b8e2f9a4d6c8b1e3a5d7f9c0b2e4a6d8f0c"

	for _, c := range []struct{ version, want string }{
		{"v1.4.0", sourceRepo + "/tree/v1.4.0"},
		{"main-" + sha, sourceRepo + "/tree/" + sha},
		{"dev", sourceRepo},
		{"", sourceRepo},
		// Nothing a version string says can steer the path, however it was stamped in.
		{"main-../../elsewhere", sourceRepo},
	} {
		if got := sourceURL(c.version); got != c.want {
			t.Errorf("version %q offers %q, want %q", c.version, got, c.want)
		}
	}
}
