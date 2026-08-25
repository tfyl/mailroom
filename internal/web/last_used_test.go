package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
)

// present drives the grant through the path an MCP client's bearer token takes, which is
// the only thing that records a use.
func present(t *testing.T, db *store.Store, id grant.ID) {
	t.Helper()
	ctx := context.Background()

	const token = "a-real-bearer-token"
	if err := db.IssueToken(ctx, token, id, nil); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	if _, err := oauthsrv.New(db, "https://mail.example.com").GrantForRequest(ctx, r); err != nil {
		t.Fatalf("the token should have resolved: %v", err)
	}
}

// lastUsedLine returns the part of the card that says when the grant was last used, which is
// the whole of what this is about and small enough to read in a failure.
//
// It anchors on "Last used" rather than on the dates line beside it. When two grants carry the
// same client-supplied label, when each was last used is the only thing that says which one a
// client is holding — so it stopped being a date in the small print and became a labelled fact
// on the card, alongside the mailboxes it reaches.
func lastUsedLine(t *testing.T, body string) string {
	t.Helper()
	at := strings.Index(body, "Last used")
	if at < 0 {
		t.Fatalf("no last-used fact on the page at all: %s", body)
	}
	end := min(at+400, len(body))
	return strings.Join(strings.Fields(body[at:end]), " ")
}

// The report was that the grants page always says "never used". It did, for every grant, on
// every instance: nothing wrote last_used_at, so the page was reading a column that was
// never set rather than one that happened to be empty.
func TestTheGrantsPageShowsWhenAGrantWasLastUsed(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)

	unused := lastUsedLine(t, renderAs(t, s.grants, "/grants", me))
	t.Logf("never used: %s", unused)
	if !strings.Contains(unused, "never used") {
		t.Errorf("a grant nothing has presented should say so: %s", unused)
	}

	present(t, db, id)

	g, err := db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.LastUsedAt == nil {
		t.Fatal("presenting the token recorded no use")
	}

	used := lastUsedLine(t, renderAs(t, s.grants, "/grants", me))
	t.Logf("used: %s", used)
	if strings.Contains(used, "never used") {
		t.Errorf("a grant that has been used still reads as never used: %s", used)
	}
	if want := g.LastUsedAt.Format("2 Jan 15:04"); !strings.Contains(used, want) {
		t.Errorf("the page does not show %q: %s", want, used)
	}
}

// The same value on the confirmation page, which is where somebody deciding whether to revoke
// a grant is actually looking.
func TestTheRevokeConfirmationShowsWhenAGrantWasLastUsed(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)
	present(t, db, id)

	g, err := db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	body := postRevoke(s, me, url.Values{"id": {string(id)}}).Body.String()
	if strings.Contains(body, "never used") {
		t.Errorf("the confirmation page reads as never used: %s", lastUsedLine(t, body))
	}
	if want := g.LastUsedAt.Format("2 Jan 15:04"); !strings.Contains(body, want) {
		t.Errorf("the confirmation page does not show %q: %s", want, lastUsedLine(t, body))
	}
}
