package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Where a mode is set, and what setting one costs.
//
// It lives on the grant because a client holds exactly one, and because a per-session setting
// would vanish when a client reconnected — which is the one moment an operator is least
// likely to be watching. That choice puts it on three screens: the consent form that creates
// a grant, the grants page that shows what one is doing, and the edit page that changes it.
// These prove that changing it behaves like changing a scope, in both directions.

// approve submits the consent form, and onlyGrant reads back what it recorded. The mode is
// the one field on that screen with a default, so what it lands on when nobody touches it is
// worth asserting rather than assuming.
func (rig consentRig) approve(t *testing.T, as user.User, form url.Values) {
	t.Helper()
	rec := rig.post(t, rig.oauth.Approve, "/authorize/approve", as, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: want a redirect, got %d: %s", rec.Code, rec.Body)
	}
}

func (rig consentRig) onlyGrant(t *testing.T) *grant.Grant {
	t.Helper()
	grants, err := rig.db.ListGrants(context.Background(), rig.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("want one grant, got %d", len(grants))
	}
	return grants[0]
}

// A new grant gets the middle setting unless somebody picks another, so an operator who never
// reads that part of the consent screen gets an agent that is told to ask.
func TestANewGrantIsBalancedUnlessTheOperatorSaysOtherwise(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rig.approve(t, rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Claude"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "send"},
		"expires_days": {"90"},
	})

	g := rig.onlyGrant(t)
	if g.Mode.Resolved() != grant.ModeConfirm {
		t.Errorf("a grant approved without a mode should be %q, got %q",
			grant.ModeConfirm, g.Mode.Resolved())
	}
}

// The screen offers all three, with the default already selected — because a grant has a mode
// whether or not anybody chooses one, so an empty radiogroup would misdescribe what Approve
// does. It is also the one group on that page that arrives with anything selected, which is
// worth pinning: nothing else there may be pre-granted.
func TestTheConsentScreenOffersTheThreeModesWithTheDefaultSelected(t *testing.T) {
	rig := newConsentRig(t)
	body, _ := rig.open(t, rig.ada)

	for _, m := range grant.AllModes {
		if !hasCheckbox(body, "mode", string(m)) {
			t.Errorf("the consent screen does not offer %q", m)
		}
		want := m == grant.DefaultMode
		if got := ticked(t, body, "mode", string(m)); got != want {
			t.Errorf("mode %q: selected = %v, want %v", m, got, want)
		}
	}
	// Nothing else on this screen may arrive selected, which is what the mode radios must
	// not quietly change.
	for _, c := range mail.AllCapabilities {
		if ticked(t, body, "capabilities", string(c)) {
			t.Errorf("the capability %q arrived ticked on a fresh consent screen", c)
		}
	}
}

// A mode chosen on the consent screen is the one recorded.
func TestTheModeChosenOnTheConsentScreenIsTheOneRecorded(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rig.approve(t, rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Claude"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "send"},
		"mode":         {"hold"},
		"expires_days": {"90"},
	})

	if g := rig.onlyGrant(t); g.Mode != grant.ModeHold {
		t.Errorf("want the mode the operator picked, got %q", g.Mode)
	}
}

// A mode this build does not have is refused rather than resolved. A form naming one has
// drifted from the server, and landing quietly on the default would hide that while leaving
// the operator believing they had set what they picked.
func TestAnUnknownModeIsRefusedAtConsent(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Approve, "/authorize/approve", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Claude"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read"},
		"mode":         {"paranoid"},
		"expires_days": {"90"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want the approval refused, got %d: %s", rec.Code, rec.Body)
	}
	if grants, _ := rig.db.ListGrants(context.Background(), rig.ada.ID); len(grants) != 0 {
		t.Error("a refused approval recorded a grant anyway")
	}
}

// The select-all round trip must carry the mode back like every other choice, or pressing one
// of those buttons would silently reset an operator's answer to the default.
func TestReselectKeepsTheChosenMode(t *testing.T) {
	rig := newConsentRig(t)
	_, requestID := rig.open(t, rig.ada)

	rec := rig.post(t, rig.oauth.Reselect, "/authorize/reselect", rig.ada, url.Values{
		"request_id":   {requestID},
		"label":        {"Claude"},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read"},
		"mode":         {"hold"},
		"reselect":     {"all-mailboxes"},
		"expires_days": {"90"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reselect: %d %s", rec.Code, rec.Body)
	}
	if !ticked(t, rec.Body.String(), "mode", "hold") {
		t.Error("the select-all round trip lost the mode the operator had chosen")
	}
}

// Tightening a mode takes autonomy away, and takes it away at once. It is the same shape as
// removing a capability, and gets the same treatment: no confirmation, applied on save.
func TestTighteningTheModeAppliesStraightAway(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_tighten",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapSend), nil)

	rec := rig.submit(t, rig.ada, url.Values{
		"id":           {string(id)},
		"accounts":     {"acct_ada_work"},
		"capabilities": {"read", "send"},
		"mode":         {"hold"},
		"expires_days": {"keep"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tightening should apply without a confirmation, got %d: %s", rec.Code, rec.Body)
	}

	g, err := rig.db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode != grant.ModeHold {
		t.Errorf("the mode was not tightened: %q", g.Mode)
	}
}

// Loosening one hands the client more initiative than it had, with nobody at that end asked
// again. That is the shape widening a scope has, so it is confirmed the same way — and the
// page has to itemise what is being handed over rather than asking in the abstract.
func TestLooseningTheModeAsksFirst(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_loosen",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapSend), nil)

	// Start it strict, without ceremony.
	rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "send"}, "mode": {"hold"}, "expires_days": {"keep"},
	})

	proposal := url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "send"}, "mode": {"unattended"}, "expires_days": {"keep"},
	}
	asked := rig.submit(t, rig.ada, proposal)
	if asked.Code != http.StatusOK {
		t.Fatalf("loosening should ask first, got %d: %s", asked.Code, asked.Body)
	}
	body := asked.Body.String()
	for _, want := range []string{"It gains", "unattended", "Set it to unattended",
		"does not release what is already waiting"} {
		if !strings.Contains(body, want) {
			t.Errorf("the question should say %q:\n%s", want, body)
		}
	}

	g, err := rig.db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode != grant.ModeHold {
		t.Fatalf("asking the question loosened the grant anyway: %q", g.Mode)
	}

	confirmed := proposal
	confirmed.Set("confirm", "yes")
	if rec := rig.submit(t, rig.ada, confirmed); rec.Code != http.StatusSeeOther {
		t.Fatalf("the confirmed edit should apply, got %d: %s", rec.Code, rec.Body)
	}
	if g, _ = rig.db.Grant(context.Background(), id); g.Mode != grant.ModeUnattended {
		t.Errorf("the confirmed edit did not take: %q", g.Mode)
	}
}

// Setting an old grant explicitly to the mode it already behaves as is not a change, in either
// direction. Without that, every such edit would ask the operator to confirm a difference they
// cannot see and did not make.
func TestRecordingTheModeAGrantAlreadyBehavesAsIsNotAChange(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_nomode",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead), nil)

	g, err := rig.db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode != "" {
		t.Fatalf("this fixture is meant to have no mode recorded, got %q", g.Mode)
	}

	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read"}, "mode": {"confirm"}, "expires_days": {"keep"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nothing to change") {
		t.Errorf("want the edit reported as no change, got %d: %s", rec.Code, rec.Body)
	}
}

// Changing a mode changes what an already-issued token may do on its own, which is exactly
// what the audit log is for. Both directions are recorded: tightening needs no confirmation
// and is still a change somebody may need to find later.
func TestTheAuditLogRecordsAModeChange(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_audit_mode",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapSend), nil)

	rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "send"}, "mode": {"hold"}, "expires_days": {"keep"},
	})

	entries, err := rig.db.RecentAudit(context.Background(), rig.ada.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, e := range entries {
		if e.Tool == "grant.edit" && strings.HasPrefix(e.Outcome, "mode ") {
			found = e.Outcome
		}
	}
	if found == "" {
		t.Fatalf("the mode change is not in the audit log: %+v", entries)
	}
	if !strings.Contains(found, "confirm") || !strings.Contains(found, "hold") {
		t.Errorf("the row should say what it moved from and to, got %q", found)
	}
}

// A grant that predates modes has nothing recorded, and editing something else about it must
// not quietly write a mode onto it — which would turn a default into a choice nobody made.
func TestEditingSomethingElseLeavesAnUnrecordedModeAlone(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_untouched",
		[]mail.AccountID{"acct_ada_work", "acct_ada_home"}, mail.NewSet(mail.CapRead), nil)

	rec := rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read"}, "expires_days": {"keep"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("narrowing should apply, got %d: %s", rec.Code, rec.Body)
	}

	g, err := rig.db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode != "" {
		t.Errorf("an edit that said nothing about the mode wrote %q onto the grant", g.Mode)
	}
	if g.Mode.Resolved() != grant.ModeConfirm {
		t.Errorf("and it should still behave as the default, got %q", g.Mode.Resolved())
	}
}

// The grants page has to say what each grant may do on its own beside what it may do at all.
// Somebody opening it because a client surprised them needs both halves.
func TestTheGrantsPageNamesTheMode(t *testing.T) {
	rig := newEditRig(t)
	id, _ := rig.grantFor(t, rig.ada, "grant_shown",
		[]mail.AccountID{"acct_ada_work"}, mail.NewSet(mail.CapRead, mail.CapSend), nil)
	rig.submit(t, rig.ada, url.Values{
		"id": {string(id)}, "accounts": {"acct_ada_work"},
		"capabilities": {"read", "send"}, "mode": {"hold"}, "expires_days": {"keep"},
	})

	body := rig.editPage(t, rig.ada, id).Body.String()
	if !strings.Contains(body, "set to this now") {
		t.Error("the edit page does not mark which mode the grant is set to")
	}
	for _, want := range []string{"enforced here", "wording only"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has to say which modes are enforced and which are not (%q)", want)
		}
	}
}
