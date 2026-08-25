package web

import (
	"context"
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

// Editing a grant changes what an already-issued token may do, with nobody at the client end
// asked again. So these run the real handlers against the real store and then ask the real
// gate what the token can do afterwards: the only answer worth having is what a tool call
// would get, not what a row says.

type editRig struct {
	s   *Server
	db  *store.Store
	ada user.User
	bob user.User
}

func newEditRig(t *testing.T) editRig {
	t.Helper()
	ctx := context.Background()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	signInAs(s, "bob", "")

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rig := editRig{s: s, db: db}
	for _, u := range users {
		switch u.Subject {
		case "ada":
			rig.ada = u
		case "bob":
			rig.bob = u
		}
	}
	if rig.ada.ID == "" || rig.bob.ID == "" {
		t.Fatalf("both users should exist, got %+v", users)
	}

	link := func(owner user.User, id, alias string) {
		t.Helper()
		err := db.LinkAccount(ctx, owner.ID, mail.Account{
			ID: mail.AccountID(id), Alias: alias, Address: alias + "@example.com",
			Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
		}, "sealed", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	link(rig.ada, "acct_ada_work", "ada-work")
	link(rig.ada, "acct_ada_home", "ada-home")
	link(rig.bob, "acct_bob_work", "bob-work")

	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	return rig
}

// grantFor records a grant and hands back a bearer token for it, because the question every
// test here asks is what the client's token can do afterwards.
func (rig editRig) grantFor(t *testing.T, owner user.User, id string, accounts []mail.AccountID, caps mail.Set, expires *time.Time) (grant.ID, string) {
	t.Helper()
	ctx := context.Background()

	g := &grant.Grant{
		ID: grant.ID(id), OwnerID: owner.ID, ClientID: "client_1", Label: "An agent",
		Accounts: accounts, Caps: caps, ExpiresAt: expires,
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

func (rig editRig) editPage(t *testing.T, as user.User, id grant.ID) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/grants/edit?id="+url.QueryEscape(string(id)), nil)
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.editGrantForm(rec, r)
	return rec
}

func (rig editRig) submit(t *testing.T, as user.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/grants/edit", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.editGrant(rec, r)
	return rec
}

// allows asks the gate what a token may do, resolving it exactly as an MCP call would: token
// to grant, grant to accounts, grant against the capability.
func (rig editRig) allows(t *testing.T, token, alias string, c mail.Capability) error {
	t.Helper()
	ctx := context.Background()

	g, err := rig.db.GrantForToken(ctx, token)
	if err != nil {
		return err
	}
	gate := grant.NewGate(rig.db, rig.db, rig.db)
	_, err = gate.Resolve(ctx, g, "mail.search", []string{alias}, c)
	return err
}

// The safe direction. Nothing about it needs confirming, and it has to reach the token
// immediately or the operator has not actually taken anything away.
func TestNarrowingAGrantIsRefusedByTheGateStraightAway(t *testing.T) {
	rig := newEditRig(t)
	id, token := rig.grantFor(t, rig.ada, "grant_narrow",
		[]mail.AccountID{"acct_ada_work", "acct_ada_home"},
		mail.NewSet(mail.CapRead, mail.CapSend), nil)

	if err := rig.allows(t, token, "ada-work", mail.CapSend); err != nil {
		t.Fatalf("the grant should start out able to send: %v", err)
	}

	rec := rig.submit(t, rig.ada, url.Values{
		"id":           {string(id)},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read"},
		"expires_days": {"keep"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("narrowing should apply without a confirmation, got %d: %s", rec.Code, rec.Body)
	}

	if err := rig.allows(t, token, "ada-work", mail.CapSend); err == nil {
		t.Error("send was removed and the gate still allows it")
	}
	if err := rig.allows(t, token, "ada-work", mail.CapRead); err != nil {
		t.Errorf("read was kept and the gate refuses it: %v", err)
	}
	if err := rig.allows(t, token, "ada-home", mail.CapRead); err == nil {
		t.Error("ada-home was removed from the grant and the gate still reaches it")
	}
}

// Widening is the act the confirmation exists for: the client's token starts using it on the
// next call and nobody at that end approved anything.
func TestWideningAsksFirstAndThenTakesEffect(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, token := rig.grantFor(t, rig.ada, "grant_widen",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	proposal := url.Values{
		"id":           {string(id)},
		"accounts":     {"acct_ada_work", "acct_ada_home"},
		"capabilities": {"read", "send"},
		"expires_days": {"keep"},
	}

	asked := rig.submit(t, rig.ada, proposal)
	if asked.Code != http.StatusOK {
		t.Fatalf("widening should ask first, got %d: %s", asked.Code, asked.Body)
	}
	body := asked.Body.String()
	for _, want := range []string{"It gains", "send", "ada-home", "Grant send"} {
		if !strings.Contains(body, want) {
			t.Errorf("the question should itemise what is being handed over (%q): %s", want, body)
		}
	}

	g, err := rig.db.Grant(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Caps.Has(mail.CapSend) {
		t.Fatal("asking the question widened the grant anyway")
	}
	if err := rig.allows(t, token, "ada-work", mail.CapSend); err == nil {
		t.Fatal("asking the question let the token send")
	}

	confirmed := proposal
	confirmed.Set("confirm", "yes")
	if rec := rig.submit(t, rig.ada, confirmed); rec.Code != http.StatusSeeOther {
		t.Fatalf("a confirmed widening should apply, got %d: %s", rec.Code, rec.Body)
	}

	if err := rig.allows(t, token, "ada-work", mail.CapSend); err != nil {
		t.Errorf("send was added and the gate still refuses it: %v", err)
	}
	if err := rig.allows(t, token, "ada-home", mail.CapRead); err != nil {
		t.Errorf("ada-home was added and the gate does not reach it: %v", err)
	}
}

// The boundary the whole product is built around. Somebody else's grant is not editable, and
// the refusal must not confirm that the id names anything.
func TestAnotherUsersGrantCannotBeEdited(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, _ := rig.grantFor(t, rig.ada, "grant_ada",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	for _, attempt := range []struct {
		name string
		form url.Values
	}{
		{"straight at it", url.Values{
			"id": {string(id)}, "accounts": {"acct_bob_work"},
			"capabilities": {"read", "send", "destructive"}, "confirm": {"yes"},
		}},
		{"only narrowing, which needs no confirmation", url.Values{
			"id": {string(id)}, "accounts": {"acct_ada_work"}, "capabilities": {"read"},
		}},
	} {
		rec := rig.submit(t, rig.bob, attempt.form)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404 for somebody else's grant, got %d: %s", attempt.name, rec.Code, rec.Body)
		}
		for _, leak := range []string{"An agent", "acct_ada_work", "ada-work"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("%s: the refusal names %q, which confirms the grant exists: %s",
					attempt.name, leak, rec.Body)
			}
		}
	}

	if rec := rig.editPage(t, rig.bob, id); rec.Code != http.StatusNotFound {
		t.Fatalf("the edit form for somebody else's grant should be a 404, got %d: %s", rec.Code, rec.Body)
	}

	g, err := rig.db.Grant(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Accounts) != 1 || g.Accounts[0] != "acct_ada_work" || g.Caps.Len() != 1 {
		t.Fatalf("the grant changed under somebody else's hand: %+v", g)
	}
}

// The id of a mailbox is not a secret — it appears in the audit log and in tool results — so
// posting one straight at the endpoint is the obvious thing to try. Ownership is checked
// where the write happens rather than only where the form is drawn.
func TestAMailboxYouDoNotOwnCannotBeAttached(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, token := rig.grantFor(t, rig.ada, "grant_scope",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	rec := rig.submit(t, rig.ada, url.Values{
		"id":           {string(id)},
		"accounts":     {"acct_ada_work", "acct_bob_work"},
		"capabilities": {"read"},
		"expires_days": {"keep"},
		"confirm":      {"yes"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("attaching somebody else's mailbox was accepted: %s", rec.Header().Get("Location"))
	}

	g, err := rig.db.Grant(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range g.Accounts {
		if a == "acct_bob_work" {
			t.Fatalf("the grant now names a mailbox its owner does not own: %+v", g.Accounts)
		}
	}
	// And the gate agrees, which is the thing that would actually have leaked mail.
	if err := rig.allows(t, token, "bob-work", mail.CapRead); err == nil {
		t.Fatal("ada's token can read bob's mailbox")
	}
}

// Expiry is a security control like any other scope, so it is editable in both directions —
// and a grant that has run out stays refused until somebody deliberately moves it.
func TestExpiryIsEditableAndAnExpiredGrantStaysRefused(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()

	year := time.Now().Add(365 * 24 * time.Hour)
	id, token := rig.grantFor(t, rig.ada, "grant_expiry",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), &year)

	// Bringing it forward narrows, so it applies without a confirmation.
	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read"}, "expires_days": {"7"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("shortening an expiry should apply, got %d: %s", rec.Code, rec.Body)
	}
	g, err := rig.db.Grant(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if g.ExpiresAt == nil || g.ExpiresAt.After(time.Now().Add(8*24*time.Hour)) {
		t.Fatalf("expiry should now be about a week out, got %v", g.ExpiresAt)
	}
	if err := rig.allows(t, token, "ada-work", mail.CapRead); err != nil {
		t.Errorf("a grant expiring in a week is still live: %v", err)
	}

	// One that has already run out. Nothing about editing makes it usable again by itself.
	past := time.Now().Add(-24 * time.Hour)
	deadID, deadToken := rig.grantFor(t, rig.ada, "grant_expired",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), &past)
	if err := rig.allows(t, deadToken, "ada-work", mail.CapRead); err == nil {
		t.Fatal("an expired grant was allowed")
	}

	// Narrowing it further leaves it expired: the change does not reach back and revive it.
	if rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(deadID)}, "accounts": {"acct_ada_work"}, "capabilities": {"read"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("an edit that changes nothing should come back to the form, got %d: %s", rec.Code, rec.Body)
	}
	if err := rig.allows(t, deadToken, "ada-work", mail.CapRead); err == nil {
		t.Fatal("an expired grant came back to life without its expiry being moved")
	}

	// Moving the expiry out is a widening, so it is confirmed, and then it works again.
	revive := url.Values{
		"id": {string(deadID)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read"}, "expires_days": {"30"},
	}
	if rec := rig.submit(t, rig.ada, revive); rec.Code != http.StatusOK {
		t.Fatalf("pushing an expiry out should ask first, got %d: %s", rec.Code, rec.Body)
	}
	revive.Set("confirm", "yes")
	if rec := rig.submit(t, rig.ada, revive); rec.Code != http.StatusSeeOther {
		t.Fatalf("a confirmed expiry change should apply, got %d: %s", rec.Code, rec.Body)
	}
	if err := rig.allows(t, deadToken, "ada-work", mail.CapRead); err != nil {
		t.Errorf("the expiry was moved out and the grant is still refused: %v", err)
	}
}

// An edit to a live grant is the only way the scope of an already-issued token moves. Reading
// the grant afterwards shows where it ended up and says nothing about it ever having moved,
// which is exactly the gap the audit log is there to close.
func TestTheAuditLogRecordsWhatAnEditChanged(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, _ := rig.grantFor(t, rig.ada, "grant_audit",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapSend), nil)

	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_home"},
		"capabilities": {"read", "labels"}, "expires_days": {"30"}, "confirm": {"yes"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the edit should have applied, got %d: %s", rec.Code, rec.Body)
	}

	entries, err := rig.db.RecentAudit(ctx, rig.ada.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, e := range entries {
		if e.Tool == "grant.edit" {
			found[e.Outcome] = e.Account
		}
	}
	if len(found) == 0 {
		t.Fatalf("an edit to a live grant went unrecorded: %+v", entries)
	}
	if found["mailbox added"] != "ada-home" {
		t.Errorf("the added mailbox should be recorded and shown by its alias: %+v", found)
	}
	if found["mailbox removed"] != "ada-work" {
		t.Errorf("the removed mailbox should be recorded: %+v", found)
	}
	if _, ok := found["capabilities +labels -send"]; !ok {
		t.Errorf("the capability change should be recorded in both directions: %+v", found)
	}
	var expiry bool
	for outcome := range found {
		if strings.HasPrefix(outcome, "expiry ") {
			expiry = true
		}
	}
	if !expiry {
		t.Errorf("the expiry change should be recorded: %+v", found)
	}
}

// The point of the whole feature: the client keeps the token it already has. If an edit
// dropped it, editing would be revoking with extra steps.
func TestEditingDoesNotInvalidateTheClientsToken(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, token := rig.grantFor(t, rig.ada, "grant_token",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "draft"}, "expires_days": {"keep"}, "confirm": {"yes"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the edit should have applied, got %d: %s", rec.Code, rec.Body)
	}

	g, err := rig.db.GrantForToken(ctx, token)
	if err != nil {
		t.Fatalf("the token the client already holds stopped resolving: %v", err)
	}
	if g.ID != id {
		t.Fatalf("the token now resolves to %s rather than %s", g.ID, id)
	}
	if !g.Caps.Has(mail.CapDraft) {
		t.Error("the token resolves to a grant that did not pick up the edit")
	}
}

// A revoked grant is not editable. Revoking is documented as the thing that cannot be undone,
// and an edit that brought one back would make that untrue.
func TestARevokedGrantCannotBeEdited(t *testing.T) {
	rig := newEditRig(t)
	ctx := context.Background()
	id, _ := rig.grantFor(t, rig.ada, "grant_revoked",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)
	if err := rig.db.RevokeGrant(ctx, rig.ada.ID, id); err != nil {
		t.Fatal(err)
	}

	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "send"}, "confirm": {"yes"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for a revoked grant, got %d: %s", rec.Code, rec.Body)
	}
	g, err := rig.db.Grant(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Caps.Has(mail.CapSend) || !g.Revoked() {
		t.Fatalf("a revoked grant was edited: %+v", g)
	}
}

// Both new pages are forms that change permissions, so both have to carry the field csrf.check
// actually reads. Counting is what catches a form added later without one.
func TestTheEditPagesCarryTheCSRFFieldNameThatIsChecked(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_csrf",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	widen := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"}, "capabilities": {"read", "send"},
	})
	pages := map[string]string{
		"/grants/edit":           rig.editPage(t, rig.ada, id).Body.String(),
		"the widen confirmation": widen.Body.String(),
	}
	for name, body := range pages {
		forms := strings.Count(body, "<form")
		if forms == 0 {
			t.Errorf("%s: rendered no forms at all, so this proves nothing", name)
		}
		if fields := strings.Count(body, `name="csrf_token"`); forms != fields {
			t.Errorf("%s: every form needs a csrf_token field: %d forms, %d fields", name, forms, fields)
		}
	}
}

// Prints the markup the operator is actually served, so the two new pages can be read rather
// than inferred from the handlers.
func TestTheGrantPagesRenderAsPlainForms(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_render",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	list := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/grants", nil)
	r = r.WithContext(user.NewContext(r.Context(), rig.ada))
	rig.s.grants(list, r)

	widen := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work", "acct_ada_home"},
		"capabilities": {"read", "send", "destructive"}, "expires_days": {"365"},
	})

	for name, body := range map[string]string{
		"GET /grants":                  list.Body.String(),
		"GET /grants/edit":             rig.editPage(t, rig.ada, id).Body.String(),
		"POST /grants/edit (widening)": widen.Body.String(),
	} {
		t.Logf("\n===== %s =====\n%s", name, body)
		assertOnlyTheExternalScript(t, name, body)
	}

	if !strings.Contains(widen.Body.String(), "<h2>Its expiry</h2>") {
		t.Errorf("the expiry belongs in its own section, since removing one widens and adding one narrows: %s", widen.Body)
	}

	if !strings.Contains(list.Body.String(), `href="/grants/edit?id=`+string(id)) {
		t.Errorf("the grants page should offer the edit link: %s", list.Body)
	}
}

// The route back for a grant that lost draft deletion when it was split out of `draft`.
//
// Restoring it is a widening like any other, and it gets the same ceremony `send` and
// `destructive` get: the button names it. The test for that group is not how alarming a
// capability looks but whether taking it back afterwards reaches what was done under it, and
// a discarded draft is not coming back any more than a sent message is.
func TestWideningToRestoreDraftDeletionNamesIt(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_predates_the_split",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapDraft), nil)

	asked := rig.submit(t, rig.ada, url.Values{
		"id":           {string(id)},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "draft", "discard"},
		"expires_days": {"keep"},
	})
	if asked.Code != http.StatusOK {
		t.Fatalf("handing back draft deletion should ask first, got %d: %s", asked.Code, asked.Body)
	}
	body := asked.Body.String()
	for _, want := range []string{"It gains", "discard", "Grant discard", "cannot be taken back"} {
		if !strings.Contains(body, want) {
			t.Errorf("the question should name discard as irreversible (%q): %s", want, body)
		}
	}
}
