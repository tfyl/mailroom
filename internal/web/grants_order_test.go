package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// The grants page is opened to find out what has access, so a grant that has none must never
// push one that does further down it. Nothing enforces the order the store happens to return,
// which is why it is pinned here rather than left to be true by accident.
func TestOrderGrantsPutsWhatHasAccessFirst(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	day := 24 * time.Hour

	// Deliberately the worst order to be handed: the dead ones first, the never-used ahead
	// of the busy one, and the newest thing last.
	grants := []*grant.Grant{
		{ID: "revoked_recent", CreatedAt: now.Add(-2 * day), RevokedAt: ago(time.Hour), LastUsedAt: ago(2 * time.Hour)},
		{ID: "expired_old", CreatedAt: now.Add(-300 * day), ExpiresAt: ago(30 * day), LastUsedAt: ago(40 * day)},
		{ID: "live_never_old", CreatedAt: now.Add(-90 * day)},
		{ID: "expired_recent", CreatedAt: now.Add(-20 * day), ExpiresAt: ago(day), LastUsedAt: ago(2 * day)},
		{ID: "live_used_week", CreatedAt: now.Add(-200 * day), LastUsedAt: ago(7 * day)},
		{ID: "live_never_new", CreatedAt: now.Add(-time.Minute)},
		{ID: "revoked_old", CreatedAt: now.Add(-400 * day), RevokedAt: ago(100 * day)},
		{ID: "live_used_now", CreatedAt: now.Add(-365 * day), LastUsedAt: ago(time.Minute)},
	}

	orderGrants(grants, now)

	want := []grant.ID{
		// In force, most recently used first.
		"live_used_now",
		"live_used_week",
		// Then the ones nothing has ever presented, newest approval first. They are worth
		// noticing and they are not what just did something surprising.
		"live_never_new",
		"live_never_old",
		// Expired: reaching nothing, one edit from working again, so its own band.
		"expired_recent",
		"expired_old",
		// Revoked, which is over.
		"revoked_recent",
		"revoked_old",
	}
	got := make([]grant.ID, len(grants))
	for i, g := range grants {
		got[i] = g.ID
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d is %q, want %q\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

// The same property through the handler and the template, because the ordering is only worth
// anything if it survives all the way to the markup somebody reads.
func TestTheGrantsPageRendersTheBandsInOrder(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkAccount(ctx, me.ID, mail.Account{
		ID: "acct_1", Alias: "work", Address: "ada@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}, "sealed", ""); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-24 * time.Hour)
	future := time.Now().Add(90 * 24 * time.Hour)
	add := func(id, label string, expires *time.Time) grant.ID {
		g := &grant.Grant{
			ID: grant.ID(id), OwnerID: me.ID, ClientID: "client_1", Label: label,
			Accounts: []mail.AccountID{"acct_1"}, Caps: mail.NewSet(mail.CapRead),
			ExpiresAt: expires,
		}
		if err := db.CreateGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
		return g.ID
	}

	// Inserted worst-first on purpose: the revoked one before the live one, the expired one
	// in the middle.
	dead := add("g_dead", "Zapier bridge", nil)
	if err := db.RevokeGrant(ctx, me.ID, dead); err != nil {
		t.Fatal(err)
	}
	add("g_lapsed", "Weekend prototype", &past)
	add("g_live", "Claude inbox triage", &future)

	r := httptest.NewRequest(http.MethodGet, "/grants", nil)
	r = r.WithContext(user.NewContext(r.Context(), me))
	rec := httptest.NewRecorder()
	s.grants(rec, r)

	body := rec.Body.String()
	at := func(needle string) int {
		i := strings.Index(body, needle)
		if i < 0 {
			t.Fatalf("%q is not on the page at all: %s", needle, body)
		}
		return i
	}
	live, lapsed, revoked := at("Claude inbox triage"), at("Weekend prototype"), at("Zapier bridge")
	if !(live < lapsed && lapsed < revoked) {
		t.Errorf("want live before expired before revoked, got live=%d expired=%d revoked=%d:\n%s",
			live, lapsed, revoked, body)
	}
	if at("Still has access") > live {
		t.Error("the live grants are not under the heading that says they have access")
	}
	if at("<h2>Expired</h2>") > lapsed {
		t.Error("the expired grant is not under the Expired heading")
	}
}

// The failure this is written from: an operator with two active grants both called `Claude`
// added a mailbox to one of them and watched nothing happen, because their client was holding
// the other. The label is whatever the client sent and nothing keeps it unique, so the page
// has to disambiguate them itself.
func TestTwoGrantsWithOneNameAreToldApart(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	ctx := context.Background()

	signInAs(s, "ada", "")
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"work", "personal", "archive"} {
		if err := db.LinkAccount(ctx, me.ID, mail.Account{
			ID: mail.AccountID("acct_" + a), Alias: a, Address: a + "@example.com",
			Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
		}, "sealed", ""); err != nil {
			t.Fatal(err)
		}
	}

	twin := func(id string, accounts []mail.AccountID) grant.ID {
		g := &grant.Grant{
			ID: grant.ID(id), OwnerID: me.ID, ClientID: "client_1", Label: "Claude",
			Accounts: accounts, Caps: mail.NewSet(mail.CapRead),
		}
		if err := db.CreateGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	// Same name, same capabilities. Only the mailboxes and the use differ — which is exactly
	// the pair that was indistinguishable.
	held := twin("grant_01HELDBYTHECLIENT", []mail.AccountID{"acct_work", "acct_personal"})
	twin("grant_01THEOTHERONEXXXXX", []mail.AccountID{"acct_work", "acct_personal", "acct_archive"})
	present(t, db, held)

	r := httptest.NewRequest(http.MethodGet, "/grants", nil)
	r = r.WithContext(user.NewContext(r.Context(), me))
	rec := httptest.NewRecorder()
	s.grants(rec, r)
	body := rec.Body.String()

	// Each one carries the tail of its own id, so a person can match the card in front of
	// them against the page they are about to act on.
	for _, id := range []grant.ID{"grant_01HELDBYTHECLIENT", "grant_01THEOTHERONEXXXXX"} {
		if want := shortGrantID(id); !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q, the fragment that tells %s apart: %s", want, id, body)
		}
	}
	if !strings.Contains(body, "used most recently") {
		t.Errorf("nothing on the page says which of the two a client is actually holding: %s", body)
	}
	if !strings.Contains(body, "share a name with another") {
		t.Errorf("the page does not say that the name is not the identity: %s", body)
	}
	// The mark belongs to the one that was presented, not to whichever came back first.
	mark := strings.Index(body, "used most recently")
	other := strings.Index(body, shortGrantID("grant_01THEOTHERONEXXXXX"))
	if mark < 0 || other < 0 || mark > other {
		t.Errorf("the most-recently-used mark is not on the grant that was used: mark=%d other=%d\n%s",
			mark, other, body)
	}

	// And the page it sends you to names the grant unambiguously whether or not anything
	// collides, because that is the page where being one grant out is expensive.
	edit := httptest.NewRequest(http.MethodGet, "/grants/edit?id="+string(held), nil)
	edit = edit.WithContext(user.NewContext(edit.Context(), me))
	rec = httptest.NewRecorder()
	s.editGrantForm(rec, edit)
	if want := shortGrantID(held); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the edit page does not name the grant by id (%q): %s", want, rec.Body)
	}
}
