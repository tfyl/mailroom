package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// Removing a revoked grant is the one destructive-sounding action here that ends nothing: the
// access went when the grant was revoked. So these tests run the real handlers against the
// real store and ask what an operator would see afterwards — the page, and the audit log.

type removeRig struct {
	s   *Server
	db  *store.Store
	ada user.User
	bob user.User
}

func newRemoveRig(t *testing.T) removeRig {
	t.Helper()
	ctx := context.Background()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	signInAs(s, "bob", "")

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rig := removeRig{s: s, db: db}
	for _, u := range users {
		switch u.Subject {
		case "ada":
			rig.ada = u
		case "bob":
			rig.bob = u
		}
	}

	for _, a := range []struct {
		owner user.User
		id    string
		alias string
	}{{rig.ada, "acct_ada", "ada-work"}, {rig.bob, "acct_bob", "bob-work"}} {
		err := db.LinkAccount(ctx, a.owner.ID, mail.Account{
			ID: mail.AccountID(a.id), Alias: a.alias, Address: a.alias + "@example.com",
			Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
		}, "sealed", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (rig removeRig) grantFor(t *testing.T, owner user.User, id, label string, expires *time.Time) (grant.ID, string) {
	t.Helper()
	ctx := context.Background()

	account := mail.AccountID("acct_" + owner.Subject)
	g := &grant.Grant{
		ID: grant.ID(id), OwnerID: owner.ID, ClientID: "client_1", Label: label,
		Accounts: []mail.AccountID{account}, Caps: mail.NewSet(mail.CapRead), ExpiresAt: expires,
	}
	if err := rig.db.CreateGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	token := "token-for-" + id
	if err := rig.db.IssueToken(ctx, token, g.ID, nil); err != nil {
		t.Fatal(err)
	}
	return g.ID, token
}

func (rig removeRig) revoke(t *testing.T, owner user.User, id grant.ID) {
	t.Helper()
	if err := rig.db.RevokeGrant(context.Background(), owner.ID, id); err != nil {
		t.Fatal(err)
	}
}

func (rig removeRig) postRemove(t *testing.T, as user.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/grants/remove", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.removeGrant(rec, r)
	return rec
}

func (rig removeRig) postRemoveAll(t *testing.T, as user.User) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/grants/remove-all", strings.NewReader("understood=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.removeRevokedGrants(rec, r)
	return rec
}

// grantsPage renders /grants as the signed-in operator, following whatever the last redirect
// said — the removal's own feedback lives in that query string.
func (rig removeRig) grantsPage(t *testing.T, as user.User, query string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/grants"+query, nil)
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.grants(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("the grants page should render, got %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func (rig removeRig) auditPage(t *testing.T, as user.User) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/audit", nil)
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.audit(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("the audit page should render, got %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestARevokedGrantCanBeRemovedAndStopsAppearing(t *testing.T) {
	rig := newRemoveRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_1", "Zapier", nil)
	rig.revoke(t, rig.ada, id)

	if page := rig.grantsPage(t, rig.ada, ""); !strings.Contains(page, "Zapier") {
		t.Fatalf("the revoked grant should be on the page before it is removed: %s", page)
	}

	rec := rig.postRemove(t, rig.ada, url.Values{"id": {string(id)}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after removing, got %d: %s", rec.Code, rec.Body)
	}
	next := rec.Header().Get("Location")
	if next != "/grants?removed=1" {
		t.Fatalf("want the page to say what happened, got %q", next)
	}

	page := rig.grantsPage(t, rig.ada, "?removed=1")
	if strings.Contains(page, "Zapier") {
		t.Errorf("the removed grant is still on the page: %s", page)
	}
	if !strings.Contains(page, "Removed 1 revoked grant") {
		t.Errorf("the page should say the removal happened: %s", page)
	}
	// The copy is the promise, and the promise is deliberately narrow: this must not read as
	// though the record were destroyed.
	if !strings.Contains(page, "audit log still holds every row it wrote") {
		t.Errorf("the page should say what a removal does not touch: %s", page)
	}
}

// However the request is made. The button is only ever drawn on a revoked grant, so the way
// this gets tried is by posting the id — which is what this test does.
func TestALiveGrantCannotBeRemoved(t *testing.T) {
	rig := newRemoveRig(t)
	past := time.Now().Add(-48 * time.Hour)
	live, token := rig.grantFor(t, rig.ada, "grant_live", "Claude", nil)
	expired, _ := rig.grantFor(t, rig.ada, "grant_expired", "Fastmail sync", &past)

	for _, tc := range []struct {
		name string
		id   grant.ID
	}{
		{"a live grant", live},
		{"an expired grant, which is not a revoked one", expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.postRemove(t, rig.ada, url.Values{"id": {string(tc.id)}})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body)
			}
			if _, err := rig.db.Grant(context.Background(), tc.id); err != nil {
				t.Fatalf("the grant should be untouched, got %v", err)
			}
		})
	}

	// Still live in the sense that matters: the token still resolves.
	if _, err := rig.db.GrantForToken(context.Background(), token); err != nil {
		t.Errorf("the live grant's token should still work, got %v", err)
	}
	// And clearing the band takes neither of them.
	if rec := rig.postRemoveAll(t, rig.ada); rec.Header().Get("Location") != "/grants?removed=0" {
		t.Errorf("clearing with nothing revoked should remove nothing, got %q", rec.Header().Get("Location"))
	}
	page := rig.grantsPage(t, rig.ada, "")
	for _, label := range []string{"Claude", "Fastmail sync"} {
		if !strings.Contains(page, label) {
			t.Errorf("%s should still be on the page: %s", label, page)
		}
	}
}

func TestRemovingAnotherUsersGrantIsRefusedWithoutConfirmingIt(t *testing.T) {
	rig := newRemoveRig(t)
	id, _ := rig.grantFor(t, rig.bob, "grant_bob", "Bobs overnight importer", nil)
	rig.revoke(t, rig.bob, id)

	rec := rig.postRemove(t, rig.ada, url.Values{"id": {string(id)}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for somebody else's grant, got %d: %s", rec.Code, rec.Body)
	}

	// The same answer an id that never existed gets, byte for byte: anything else would
	// confirm that the id is real.
	unknown := rig.postRemove(t, rig.ada, url.Values{"id": {"grant_no_such_thing"}})
	if got, want := rec.Body.String(), unknown.Body.String(); got != want {
		t.Errorf("the refusal distinguishes a real id from a made-up one: %q vs %q", got, want)
	}
	if unknown.Code != rec.Code {
		t.Errorf("want the same status for both, got %d and %d", rec.Code, unknown.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "Bobs overnight importer") ||
		strings.Contains(body, "revoked") {
		t.Errorf("the refusal describes the grant it refused to touch: %q", body)
	}

	if _, err := rig.db.Grant(context.Background(), id); err != nil {
		t.Fatalf("bob's grant should be untouched, got %v", err)
	}
	if page := rig.grantsPage(t, rig.bob, ""); !strings.Contains(page, "Bobs overnight importer") {
		t.Errorf("bob should still see his own grant: %s", page)
	}
}

// The reason removing is a soft delete at all. A hard delete keeps every audit row and blanks
// the name on all of them, and the audit page exists precisely to answer what a named client
// did — so this is asserted where an operator would read it, on the page itself.
func TestTheAuditPageStillNamesARemovedGrant(t *testing.T) {
	rig := newRemoveRig(t)
	ctx := context.Background()
	id, _ := rig.grantFor(t, rig.ada, "grant_1", "Nightly digest", nil)

	for _, e := range []grant.Audit{
		{OwnerID: rig.ada.ID, GrantID: id, AccountID: "acct_ada", Tool: "mail_search", Outcome: "ok", At: time.Now()},
		{OwnerID: rig.ada.ID, GrantID: id, AccountID: "acct_ada", Tool: "mail_send", Outcome: "scope_denied", At: time.Now()},
	} {
		if err := rig.db.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	rig.revoke(t, rig.ada, id)
	if rec := rig.postRemove(t, rig.ada, url.Values{"id": {string(id)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("removing failed: %d %s", rec.Code, rec.Body)
	}

	page := rig.auditPage(t, rig.ada)
	if strings.Count(page, "Nightly digest") < 2 {
		t.Errorf("both audit rows should still name the grant: %s", page)
	}
	for _, tool := range []string{"mail_search", "mail_send"} {
		if !strings.Contains(page, tool) {
			t.Errorf("the audit log lost the %s row: %s", tool, page)
		}
	}
}

func TestRemovingAGrantTakesItsTokensWithIt(t *testing.T) {
	rig := newRemoveRig(t)
	ctx := context.Background()
	id, token := rig.grantFor(t, rig.ada, "grant_1", "Zapier", nil)

	if _, err := rig.db.GrantForToken(ctx, token); err != nil {
		t.Fatalf("the token should resolve before any of this, got %v", err)
	}

	rig.revoke(t, rig.ada, id)
	if rec := rig.postRemove(t, rig.ada, url.Values{"id": {string(id)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("removing failed: %d %s", rec.Code, rec.Body)
	}

	if _, err := rig.db.GrantForToken(ctx, token); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("a token for a removed grant should not resolve, got %v", err)
	}
}

func TestClearingTheBandRemovesEveryRevokedGrant(t *testing.T) {
	rig := newRemoveRig(t)
	rig.grantFor(t, rig.ada, "grant_live", "Claude", nil)
	for _, label := range []string{"Zapier", "An old script", "Someone's laptop"} {
		id, _ := rig.grantFor(t, rig.ada, "grant_"+strings.ReplaceAll(label, " ", "_"), label, nil)
		rig.revoke(t, rig.ada, id)
	}

	page := rig.grantsPage(t, rig.ada, "")
	if !strings.Contains(page, "Remove 3 revoked grants") {
		t.Errorf("the page should offer to clear the whole band: %s", page)
	}
	// The tick is the guard, so it has to be in the markup and it has to be required.
	if !strings.Contains(page, `name="understood"`) || !strings.Contains(page, "required") {
		t.Errorf("the bulk removal should be behind a required tick: %s", page)
	}

	rec := rig.postRemoveAll(t, rig.ada)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect, got %d: %s", rec.Code, rec.Body)
	}
	if next := rec.Header().Get("Location"); next != "/grants?removed=3" {
		t.Fatalf("want the count in the redirect, got %q", next)
	}

	page = rig.grantsPage(t, rig.ada, "?removed=3")
	if !strings.Contains(page, "Removed 3 revoked grants") {
		t.Errorf("the page should say how many went: %s", page)
	}
	for _, label := range []string{"Zapier", "An old script", "Someone's laptop"} {
		if strings.Contains(page, label) {
			t.Errorf("%s should be gone: %s", label, page)
		}
	}
	if !strings.Contains(page, "Claude") {
		t.Errorf("the live grant should be untouched: %s", page)
	}
}

// A single grant is one press with no tick; only the button that acts on grants the operator
// may not be able to see gets one. Both are plain forms, and neither may need script.
func TestTheRemoveControlsAreOrdinaryForms(t *testing.T) {
	rig := newRemoveRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_1", "Zapier", nil)
	rig.revoke(t, rig.ada, id)

	page := rig.grantsPage(t, rig.ada, "")
	if !strings.Contains(page, `action="/grants/remove"`) {
		t.Errorf("the revoked card should carry a remove form: %s", page)
	}
	if strings.Contains(page, `action="/grants/remove-all"`) {
		t.Errorf("one revoked grant does not need a clear-the-band block: %s", page)
	}
	if forms, fields := strings.Count(page, "<form"), strings.Count(page, `name="csrf_token"`); forms != fields {
		t.Errorf("every form needs a csrf_token field: %d forms, %d fields", forms, fields)
	}
	// The disclosure the band lives in is opened by the handler after a removal, since the
	// redirect draws the page again from nothing.
	if strings.Contains(page, "<details class=\"advanced mt-8\" open>") {
		t.Errorf("the band should be closed until something happens in it: %s", page)
	}
	if opened := rig.grantsPage(t, rig.ada, "?removed=1"); !strings.Contains(opened, "<details class=\"advanced mt-8\" open>") {
		t.Errorf("the band should be open after a removal: %s", opened)
	}
}
