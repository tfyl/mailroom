//go:build shots

// Renders every page in every state this review looks at, as standalone HTML in SHOTS_DIR.
// Excluded from an ordinary build by the tag: `go test -tags shots ./internal/web -run Shots`.
package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

func TestShots(t *testing.T) {
	out := os.Getenv("SHOTS_DIR")
	if out == "" {
		t.Skip("SHOTS_DIR unset")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "app.css"), stylesheet, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "app.js"), script, 0o644); err != nil {
		t.Fatal(err)
	}

	public, _ := url.Parse("https://mail.example.com")
	ready := &config.Config{
		PublicURL: public,
		Google:    config.ProviderOAuth{ClientID: "g", ClientSecret: "s"},
		Microsoft: config.ProviderOAuth{ClientID: "m", ClientSecret: "s"},
		Zoho:      config.ProviderOAuth{ClientID: "z", ClientSecret: "s"},
	}
	s, db := testServerWith(t, signup.Policy{Mode: signup.Invite}, ready)
	signInAs(s, "ada", "")
	users, err := db.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	write := func(name, html string) {
		html = strings.ReplaceAll(html, stylesheetURL, "app.css")
		html = strings.ReplaceAll(html, scriptURL, "app.js")
		if err := os.WriteFile(filepath.Join(out, name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(html, "<no value>") || strings.Contains(html, "template:") {
			t.Errorf("%s rendered a template error", name)
		}
	}
	page := func(name, file, title, nav, url string, who user.User, data map[string]any) {
		r := httptest.NewRequest(http.MethodGet, url, nil)
		if who.ID != "" {
			r = r.WithContext(user.NewContext(r.Context(), who))
		}
		rec := httptest.NewRecorder()
		s.render(rec, r, file, title, nav, data)
		write(name, rec.Body.String())
	}

	// Pinned, so that regenerating the renders produces the same pages rather than a diff on
	// every timestamp. Nothing in the render path reads the clock: the relative wording
	// ("4 minutes ago", "expires in 6 days") is part of the fixture, because that is what the
	// handlers hand the templates.
	now := time.Date(2026, 8, 20, 14, 6, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	in := func(d time.Duration) *time.Time { at := now.Add(d); return &at }
	day := 24 * time.Hour

	// --- login ------------------------------------------------------------------------
	methods := []auth.Method{
		{ID: "google", Label: "Google", Kind: "oidc"},
		{ID: "okta", Label: "Okta", Kind: "oidc"},
	}
	page("login", "login", "Sign in", "", "/login", user.User{},
		map[string]any{"Methods": methods, "Next": "", "Error": ""})
	// The sentence is fetched from the code that produces it rather than written out here.
	// What this replaced was a literal — "Sign-in failed: oidc: token exchange refused by the
	// issuer (invalid_grant)" — and a literal handed to a template goes on rendering after the
	// product stops being able to produce it. It did: nothing has said "Sign-in failed:" and
	// nothing has quoted the issuer since the reflected error text was removed, so the shot
	// depicted a page no deployment could serve.
	//
	// access_denied is the one of the three worth a picture. It is the ordinary refusal —
	// somebody pressed Cancel at their issuer — and it is the case the allowlist exists to
	// keep sayable, so it is the only one of the three whose wording says which failure it
	// was. invalid_grant is not on the allowlist and does not arrive here anyway (it is a
	// token-endpoint code, so it reaches the page as the generic "Sign-in failed. Try again"),
	// and an unlisted code gets "Your identity provider refused the sign-in." Both are the
	// shape of a page that has been told nothing, which `login` already shows.
	page("login-error", "login", "Sign in", "", "/login", user.User{},
		map[string]any{"Methods": methods, "Next": "", "Error": loginRefusal(t, "access_denied")})
	page("login-proxy", "login", "Sign in", "", "/login", user.User{},
		map[string]any{"Methods": []auth.Method{}, "Next": "", "Error": ""})

	// --- refused ----------------------------------------------------------------------
	page("refused-invite", "refused", "Not accepting new accounts", "", "/accounts", user.User{},
		map[string]any{"Policy": signup.Invite, "NeedsInvite": true, "CanSignIn": true})
	page("refused-closed", "refused", "Not accepting new accounts", "", "/accounts", user.User{},
		map[string]any{"Policy": signup.Closed, "NeedsInvite": false, "CanSignIn": false})

	// --- accounts ---------------------------------------------------------------------
	acct := func(id, alias, addr string, p mail.ProviderID, st mail.AccountStatus, used time.Time) mail.Account {
		return mail.Account{ID: mail.AccountID(id), OwnerID: me.ID, Alias: alias, Address: addr,
			Provider: p, Status: st, LinkedAt: now.Add(-40 * day), LastUsedAt: used}
	}
	linked := []mail.Account{
		acct("acct_1", "work", "ada.lovelace@example.com", mail.ProviderGmail, mail.StatusLinked, now.Add(-3*time.Hour)),
		acct("acct_2", "personal", "ada@fastmail.example", mail.ProviderIMAP, mail.StatusNeedsReauth, now.Add(-9*day)),
		acct("acct_3", "newsletters", "ada+news@example.com", mail.ProviderZoho, mail.StatusLinked, time.Time{}),
	}
	long := append(append([]mail.Account{}, linked...),
		acct("acct_4", "quarterly_board_reporting_and_investor_updates_mailbox",
			"ada.augusta.byron.lovelace+quarterly-board-reporting@very-long-department-name.example.com",
			mail.ProviderMicrosoft, mail.StatusLinked, now.Add(-time.Minute)),
		acct("acct_5", "shared", "team@example.com", mail.ProviderMicrosoft, mail.StatusLinked, now.Add(-2*day)),
		acct("acct_6", "archive", "archive@example.com", mail.ProviderIMAP, mail.StatusLinked, now.Add(-200*day)),
	)
	accountsData := func(list []mail.Account, extra map[string]any) map[string]any {
		d := map[string]any{
			"Accounts": list, "GoogleReady": true, "ZohoReady": true, "MicrosoftReady": true,
			"User": me, "FirstRun": len(list) == 0, "IMAP": imapForm{TLS: true},
			"NoSending": false, "LinkOpen": "", "IMAPErrorField": "",
			"RenameAt": mail.AccountID(""), "RenameAlias": "", "IMAPError": "", "RenameError": "",
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	shotAccounts := func(name string, list []mail.Account, extra map[string]any) {
		page(name, "accounts", "Mailboxes", "accounts", "/accounts", me, accountsData(list, extra))
	}
	shotAccounts("accounts-empty", nil, map[string]any{"LinkOpen": "google"})
	shotAccounts("accounts-empty-unconfigured", nil, map[string]any{
		"LinkOpen": "imap", "GoogleReady": false, "ZohoReady": false, "MicrosoftReady": false})
	shotAccounts("accounts", linked, nil)
	shotAccounts("accounts-linked-nosending", linked, map[string]any{"Linked": "personal", "NoSending": true})
	shotAccounts("accounts-renamed", linked, map[string]any{"Renamed": "work"})
	shotAccounts("accounts-manage-open", linked, map[string]any{
		"RenameAt": mail.AccountID("acct_1"), "RenameAlias": "work"})
	shotAccounts("accounts-rename-error", linked, map[string]any{
		"RenameAt": mail.AccountID("acct_2"), "RenameAlias": "not a valid alias",
		"RenameError": "An alias may only contain letters, numbers, dashes and underscores."})
	shotAccounts("accounts-imap-open", linked, map[string]any{"LinkOpen": "imap"})
	shotAccounts("accounts-imap-error", linked, map[string]any{
		"LinkOpen": "imap", "IMAPErrorField": "password",
		"IMAPError": "The server refused those credentials. For Gmail this must be an app password rather than your account password.",
		"IMAP":      imapForm{Alias: "personal", Address: "ada@fastmail.example", Host: "imap.fastmail.com:993", TLS: true}})
	shotAccounts("accounts-imap-error-form", linked, map[string]any{
		"LinkOpen": "imap", "IMAPErrorField": "",
		"IMAPError": "Could not reach imap.fastmail.com:993 — dial tcp: lookup imap.fastmail.com: no such host.",
		"IMAP":      imapForm{Alias: "personal", Address: "ada@fastmail.example", Host: "imap.fastmail.com:993", TLS: false}})
	shotAccounts("accounts-google-open", linked, map[string]any{"LinkOpen": "google"})
	shotAccounts("accounts-stress", long, map[string]any{"LinkOpen": "microsoft"})
	disabled := append(append([]mail.Account{}, linked...),
		acct("acct_7", "old", "old@example.com", mail.ProviderIMAP, mail.StatusDisabled, now.Add(-300*day)))
	shotAccounts("accounts-disabled", disabled, nil)

	// --- grants -----------------------------------------------------------------------
	capOf := func(name string) capView { return capViewOf(mail.Capability(name)) }
	view := func(id, label string) grantView {
		return grantView{ID: id, Label: label, ShortID: id[len(id)-6:], CreatedAt: now.Add(-40 * day)}
	}
	claudeA := view("grant_8f21c0a94b3e77d2", "Claude")
	claudeA.Mode = modeViewOf(grant.ModeConfirm)
	claudeA.Accounts = []string{"work", "personal"}
	claudeA.Caps = []capView{capOf("read"), capOf("draft"), capOf("labels")}
	claudeA.Privileged = []capView{capOf("send")}
	claudeA.LastUsed, claudeA.LastUsedAgo = now.Add(-4*time.Minute).Format("2 Jan 15:04"), "4 minutes ago"
	claudeA.MostRecent = true
	claudeA.Ambiguous = true
	claudeA.ExpiresIn = "expires " + now.Add(6*day).Format("2 Jan 2006")
	claudeA.ExpiresWhen, claudeA.ExpiresSoon = "expires in 6 days", true

	claudeB := view("grant_1d0e4477aa91bb60", "Claude")
	claudeB.Mode = modeViewOf(grant.ModeHold)
	claudeB.Accounts = []string{"newsletters"}
	claudeB.Caps = []capView{capOf("read")}
	claudeB.LastUsed, claudeB.LastUsedAgo = now.Add(-3*day).Format("2 Jan 15:04"), "3 days ago"
	claudeB.Ambiguous = true

	nightly := view("grant_55ab19c7e30f4412", "Nightly digest")
	nightly.Mode = modeViewOf(grant.ModeUnattended)
	nightly.Accounts = []string{"work", "personal", "newsletters", "shared", "archive"}
	nightly.Caps = []capView{capOf("read"), capOf("labels")}
	nightly.Privileged = []capView{capOf("attachments")}

	importer := view("grant_77cc2201de55aa93", "Importer for the quarterly board reporting pipeline")
	importer.Mode = modeViewOf("")
	importer.Accounts = []string{"archive"}
	importer.Caps = []capView{capOf("read")}
	importer.LastUsed, importer.LastUsedAgo, importer.Idle =
		now.Add(-200*day).Format("2 Jan 15:04"), "6 months ago", true

	lapsed := view("grant_a1b2c3d4e5f60718", "Fastmail sync")
	lapsed.Mode = modeViewOf(grant.ModeConfirm)
	lapsed.Accounts = []string{"personal"}
	lapsed.Caps = []capView{capOf("read"), capOf("labels")}
	lapsed.Expired = true
	lapsed.LastUsed, lapsed.LastUsedAgo = now.Add(-70*day).Format("2 Jan 15:04"), "2 months ago"

	dead := view("grant_99ff88ee77dd66cc", "Zapier")
	dead.Mode = modeViewOf(grant.ModeConfirm)
	dead.Accounts = []string{"work"}
	dead.Caps = []capView{capOf("read")}
	dead.Revoked = true

	// The band an operator actually opens: several revoked grants, each removable on its own,
	// with the block that clears the lot underneath. Closed on every other render, so this is
	// the one state where any of that is visible.
	deadStale := view("grant_2c7d90aa14be3355", "An old laptop")
	deadStale.Accounts = []string{"work", "personal"}
	deadStale.Caps = []capView{capOf("read"), capOf("labels")}
	deadStale.Revoked = true

	deadLong := view("grant_63b0fe2288cc1104", "Quarterly Board Reporting And Investor Update Assistant")
	deadLong.Accounts = []string{"quarterly_board_reporting_and_investor_updates_mailbox"}
	deadLong.Caps = []capView{capOf("read")}
	deadLong.Revoked = true

	stress := view("grant_0011aabb22cc33dd", "Quarterly Board Reporting And Investor Update Assistant")
	stress.Mode = modeViewOf(grant.ModeHold)
	stress.Accounts = []string{"quarterly_board_reporting_and_investor_updates_mailbox", "work"}
	stress.Caps = []capView{capOf("read"), capOf("draft"), capOf("labels"), capOf("filters")}
	stress.Privileged = []capView{capOf("send"), capOf("attachments"), capOf("settings"), capOf("destructive")}
	stress.LastUsed, stress.LastUsedAgo = now.Add(-time.Hour).Format("2 Jan 15:04"), "1 hour ago"
	stress.ExpiresIn = "expires " + now.Add(2*day).Format("2 Jan 2006")
	stress.ExpiresWhen, stress.ExpiresSoon = "expires in 2 days", true

	page("grants-stress", "grants", "Grants", "grants", "/grants", me, map[string]any{
		"Live": []grantView{stress}, "Lapsed": nil, "Revoked": nil, "SharedLabels": 0})
	page("revoke-stress", "revoke", "Revoke grant", "grants", "/grants", me,
		map[string]any{"Grant": stress})

	page("grants-empty", "grants", "Grants", "grants", "/grants", me,
		map[string]any{"Live": nil, "Lapsed": nil, "Revoked": nil, "SharedLabels": 0})
	page("grants", "grants", "Grants", "grants", "/grants", me, map[string]any{
		"Live": []grantView{claudeA, claudeB, nightly, importer}, "Lapsed": []grantView{lapsed},
		"Revoked": []grantView{dead}, "SharedLabels": 2})
	page("grants-saved", "grants", "Grants", "grants", "/grants", me, map[string]any{
		"Live": []grantView{claudeA, claudeB}, "Lapsed": nil, "Revoked": nil,
		"SharedLabels": 2, "Edited": "Claude"})
	page("grants-none-live", "grants", "Grants", "grants", "/grants", me, map[string]any{
		"Live": nil, "Lapsed": []grantView{lapsed}, "Revoked": []grantView{dead}, "SharedLabels": 0})
	page("grants-revoked-open", "grants", "Grants", "grants", "/grants?removed=1", me, map[string]any{
		"Live": []grantView{claudeA, claudeB}, "Lapsed": []grantView{lapsed},
		"Revoked": []grantView{dead, deadStale, deadLong}, "SharedLabels": 2,
		"Removed": 1, "RevokedOpen": true})

	// --- revoke -----------------------------------------------------------------------
	page("revoke", "revoke", "Revoke grant", "grants", "/grants", me, map[string]any{"Grant": claudeA})
	page("revoke-long", "revoke", "Revoke grant", "grants", "/grants", me, map[string]any{"Grant": importer})

	// --- grant_edit -------------------------------------------------------------------
	editAccounts := func(list []mail.Account, checked, current map[string]bool) []accountView {
		var out []accountView
		for _, a := range list {
			out = append(out, accountView{ID: a.ID, Alias: a.Alias, Address: a.Address,
				Checked: checked[string(a.ID)], Current: current[string(a.ID)]})
		}
		return out
	}
	editCaps := func(checked, current map[string]bool) []capView {
		var out []capView
		for _, c := range mail.AllCapabilities {
			v := capViewOf(c)
			v.Checked, v.Current = checked[string(c)], current[string(c)]
			out = append(out, v)
		}
		return out
	}
	inGrant := map[string]bool{"acct_1": true, "acct_2": true}
	heldCaps := map[string]bool{"read": true, "draft": true, "labels": true, "send": true}
	editData := func(extra map[string]any) map[string]any {
		d := map[string]any{
			"Grant": claudeA, "Accounts": editAccounts(linked, inGrant, inGrant),
			"Caps":  editCaps(heldCaps, heldCaps),
			"Modes": modeViews(grant.ModeConfirm, grant.ModeConfirm), "Expires": "keep",
			"ExpiryNow": "it expires on " + now.Add(6*day).Format("2 Jan 2006"),
			"Message":   "", "Refused": false,
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	page("grant-edit", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(nil))
	page("grant-edit-refused", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Message":  "A grant needs at least one mailbox. Tick one, or revoke the grant instead.",
		"Refused":  true,
		"Accounts": editAccounts(linked, map[string]bool{}, inGrant)}))
	page("grant-edit-nothing", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Message": "Nothing to change — the grant already covers exactly that."}))
	page("grant-edit-expired", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Grant": lapsed, "ExpiryNow": "it expired on " + now.Add(-10*day).Format("2 Jan 2006"),
		"Expires": "30"}))
	page("grant-edit-no-mailboxes", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Accounts": []accountView{}}))
	page("grant-edit-tightening", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Modes": modeViews(grant.ModeHold, grant.ModeConfirm)}))
	page("grant-edit-widened", "grant_edit", "Edit grant", "grants", "/grants/edit", me, editData(map[string]any{
		"Caps": editCaps(map[string]bool{"read": true, "draft": true, "labels": true, "send": true,
			"destructive": true, "attachments": true}, heldCaps),
		"Accounts": editAccounts(long, map[string]bool{"acct_1": true, "acct_2": true, "acct_4": true}, inGrant)}))

	// --- grant_widen ------------------------------------------------------------------
	widen := grantChange{
		AddedAccounts: []accountView{{ID: "acct_4", Alias: "quarterly_board_reporting_and_investor_updates_mailbox",
			Address: "ada.augusta.byron.lovelace+quarterly-board-reporting@very-long-department-name.example.com"}},
		AddedCaps:    []capView{capOf("send"), capOf("destructive")},
		Irreversible: []string{"send", "destructive"},
	}
	page("grant-widen", "grant_widen", "Widen grant", "grants", "/grants/edit", me, map[string]any{
		"Grant": claudeA, "Change": widen, "Accounts": []mail.AccountID{"acct_1", "acct_4"},
		"Capabilities": []string{"read", "send", "destructive"}, "Mode": "confirm",
		"ExpiresDays": "keep"})

	mixed := grantChange{
		AddedAccounts:   []accountView{{ID: "acct_3", Alias: "newsletters", Address: "ada+news@example.com"}},
		AddedCaps:       []capView{capOf("attachments")},
		RemovedAccounts: []accountView{{ID: "acct_2", Alias: "personal", Address: "ada@fastmail.example"}},
		RemovedCaps:     []capView{capOf("labels")},
		Expiry:          "it will expire on " + now.Add(365*day).Format("2 Jan 2006") + " rather than on " + now.Add(6*day).Format("2 Jan 2006"),
		ExpiryWidens:    true,
		Irreversible:    []string{"attachments"},
	}
	page("grant-widen-mixed", "grant_widen", "Widen grant", "grants", "/grants/edit", me, map[string]any{
		"Grant": claudeA, "Change": mixed, "Accounts": []mail.AccountID{"acct_1", "acct_3"},
		"Capabilities": []string{"read", "attachments"}, "Mode": "confirm", "ExpiresDays": "365"})

	narrowExpiry := grantChange{
		AddedCaps: []capView{capOf("draft")},
		Expiry:    "it will expire on " + now.Add(7*day).Format("2 Jan 2006") + " rather than never",
	}
	page("grant-widen-expiry", "grant_widen", "Widen grant", "grants", "/grants/edit", me, map[string]any{
		"Grant": nightly, "Change": narrowExpiry, "Accounts": []mail.AccountID{"acct_1"},
		"Capabilities": []string{"read", "draft"}, "Mode": "confirm", "ExpiresDays": "7"})

	// The widening that hands over no capability at all: coming off `hold`, where the client
	// gains nothing it did not already hold and starts using it without anybody agreeing to
	// each use. It is the state this page had no shape for before modes existed.
	loosened := grantChange{
		ModeChanged: true, ModeLoosens: true,
		ModeFrom: modeViewOf(grant.ModeHold), ModeTo: modeViewOf(grant.ModeUnattended),
	}
	strictClaude := claudeA
	strictClaude.Mode = modeViewOf(grant.ModeHold)
	page("grant-widen-mode", "grant_widen", "Widen grant", "grants", "/grants/edit", me, map[string]any{
		"Grant": strictClaude, "Change": loosened, "Accounts": []mail.AccountID{"acct_1", "acct_2"},
		"Capabilities": []string{"read", "draft", "labels", "send"}, "Mode": "unattended",
		"ExpiresDays": "keep"})

	// --- held -----------------------------------------------------------------------
	//
	// The page a `hold` grant is answered on. Three shapes worth looking at: an ordinary
	// message waiting, one whose grant has since been revoked, and one of the other kinds
	// whose summary is the whole story.
	heldRowOf := func(id, summary, kind, label, act, account, grantLabel, waiting, at string) heldRow {
		return heldRow{ID: id, Summary: summary, Kind: kind, KindLabel: label,
			Act: act, Account: account, Grant: grantLabel, Waiting: waiting, At: at}
	}

	// MAILROOM_HELD_TTL's default, which is what an unconfigured install has and so what the
	// page shows unless somebody turned retention off. Both strings are put through the
	// functions heldQueue puts them through — humanUntil for the row, spanOf for the
	// paragraph under it — rather than written out, so a change to either wording moves the
	// picture with it instead of leaving the fixture claiming a page that reads differently.
	heldTTL := 72 * time.Hour
	expires := func(queued time.Time) string { return humanUntil(queued.Add(heldTTL), now) }
	retention := spanOf(heldTTL)

	waitingSend := heldRowOf("held_1",
		"send Re: the Q3 numbers to finance@example.com, board@example.com",
		"send", "send", "send", "work", "Claude", "12 minutes ago",
		now.Add(-12*time.Minute).Format("2 Jan 15:04"))
	waitingSend.To = []string{"finance@example.com", "\"The board\" <board@example.com>"}
	waitingSend.Cc = []string{"ada.lovelace@example.com"}
	waitingSend.Subject = "Re: the Q3 numbers"
	waitingSend.Attachments = []string{"q3-summary.pdf"}
	waitingSend.Body = "Hi both,\n\nThe reconciled figures are attached. The variance in " +
		"September is the reclassified hosting spend we talked about, not a new cost.\n\n" +
		"I have not changed anything in the commentary.\n\nAda"
	waitingSend.Expires = expires(now.Add(-12 * time.Minute))

	waitingDelete := heldRowOf("held_2", "delete 214 messages in newsletters",
		"trash", "delete", "delete them", "newsletters", "Nightly digest", "2 days ago",
		now.Add(-2*day).Format("2 Jan 15:04"))
	// Queued two days ago against a three-day retention, so this is the row nearest going.
	// The queue is worked oldest first, which is why the expiry is on the row and not only in
	// the prose: the one at the top is the one with the least time left.
	waitingDelete.Expires = expires(now.Add(-2 * day))

	orphaned := heldRowOf("held_3",
		"turn on the vacation responder on quarterly_board_reporting_and_investor_updates_mailbox, replying Out of office until 3 September",
		"set_vacation", "vacation responder", "set it",
		"quarterly_board_reporting_and_investor_updates_mailbox",
		"Quarterly Board Reporting And Investor Update Assistant", "6 hours ago",
		now.Add(-6*time.Hour).Format("2 Jan 15:04"))
	orphaned.GrantRevoked = true
	orphaned.Expires = expires(now.Add(-6 * time.Hour))

	answered := []heldRow{
		{ID: "held_9", Summary: "send Re: lunch to sam@example.com", KindLabel: "send",
			Account: "work", Grant: "Claude", Resolution: "done",
			Resolved: now.Add(-time.Hour).Format("2 Jan 15:04")},
		{ID: "held_8", Summary: "send an apology to everyone@example.com", KindLabel: "send",
			Account: "work", Grant: "Claude", Resolution: "discarded",
			Resolved: now.Add(-3 * time.Hour).Format("2 Jan 15:04")},
		// The resolution nobody chose. An action that expires is resolved rather than
		// deleted: the message goes and the line it was listed by stays, so this row is the
		// whole of what retention leaves behind and the only place on the page it can be
		// seen.
		{ID: "held_7", Summary: "send the weekly figures to team@example.com", KindLabel: "send",
			Account: "work", Grant: "Nightly digest", Resolution: held.Expired,
			Resolved: now.Add(-4 * day).Format("2 Jan 15:04")},
	}

	// Rendered before anything is put in the store, so the nav carries no count beside it —
	// which is the state an empty queue is actually in.
	page("held-empty", "held", "Held", "held", "/held", me,
		map[string]any{"Pending": nil, "Recent": nil})

	// The count in the nav is derived rather than passed, because it is on every page and a
	// number the caller supplied would be a number that could disagree with the queue. So the
	// pages below are rendered with two actions genuinely waiting.
	//
	// Queued at the wall clock rather than at the pinned `now`, and that is load-bearing since
	// held actions started expiring: the count comes from a query with a cutoff of
	// time.Now() minus the retention, so a row stamped with a date in the fixture is a row
	// already expired on any day but the one the fixture was written. It went that way
	// quietly — the badge simply stopped being drawn on every page in this set. Nothing
	// renders this timestamp, so nothing about these pages is unpinned by using the real one.
	for i, summary := range []string{waitingSend.Summary, waitingDelete.Summary} {
		err := db.HoldAction(t.Context(), me.ID, held.Action{
			ID: fmt.Sprintf("held_shot_%d", i), OwnerID: me.ID, GrantID: "grant_8f21c0a94b3e77d2",
			AccountID: "acct_1", Tool: "mail.send", Kind: held.KindSend,
			Summary: summary, Payload: []byte("{}"), CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Retention is on every state that has something waiting, because it is on every such page
	// heldQueue draws unless MAILROOM_HELD_TTL is off. What it says is why the expiry is
	// worth photographing at all: a held send is a whole message sitting unencrypted in this
	// server's database, and the paragraph is where the page admits how long that lasts.
	page("held", "held", "Held", "held", "/held", me, map[string]any{
		"Pending": []heldRow{waitingSend, waitingDelete}, "Recent": answered,
		"Retention": retention})
	page("held-done", "held", "Held", "held", "/held", me, map[string]any{
		"Pending": []heldRow{waitingDelete}, "Recent": answered, "Retention": retention,
		"Done": "Done — send Re: the Q3 numbers to finance@example.com."})
	page("held-failed", "held", "Held", "held", "/held", me, map[string]any{
		"Pending": []heldRow{waitingSend}, "Recent": answered, "Retention": retention,
		"Failed": "The mail server refused the message: 550 5.7.1 relay access denied."})
	page("held-stress", "held", "Held", "held", "/held", me, map[string]any{
		"Pending": []heldRow{orphaned}, "Recent": nil, "Retention": retention})

	// The closed-actions list, disclosed. Every state above carries it and none of them shows
	// it: the summary line is on the page as served and the disclosure is shut, so what is
	// inside has never been rendered into this set at all. It is where an action goes when it
	// is answered and where it goes when nobody answers it, which is the half of retention
	// that is not the expiry on the row above. Derived by opening the disclosure, the same way
	// audit-open is and for the same reason: no handler has cause to force it open, so there
	// is no data to render it from.
	if b, err := os.ReadFile(filepath.Join(out, "held.html")); err == nil {
		opened := strings.Replace(string(b), `<details class="advanced mt-8">`,
			`<details class="advanced mt-8" open>`, 1)
		if opened == string(b) {
			t.Error("held.html has no closed-actions disclosure to open")
		}
		if err := os.WriteFile(filepath.Join(out, "held-open.html"), []byte(opened), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// --- audit ------------------------------------------------------------------------
	//
	// The states worth looking at are the ordinary row, the two that read as trouble and are
	// not the same trouble — a refusal by the gate and a mailbox that failed — the send, which
	// is the row carrying the most, and a row written before any of that was recorded.
	row := func(tm, g, a, tool, outcome string, refused, changed bool) auditRow {
		return auditRow{Time: tm, Grant: g, Account: a, Tool: tool, Outcome: outcome,
			Refused: refused, Changed: changed}
	}
	detailed := func(r auditRow, capability, affected string, d auditRow) auditRow {
		r.Capability, r.Affected = capability, affected
		r.Targets, r.More, r.Action, r.Name = d.Targets, d.More, d.Action, d.Name
		r.To, r.Cc, r.Bcc, r.Subject = d.To, d.Cc, d.Bcc, d.Subject
		r.Reason, r.FromDraft = d.Reason, d.FromDraft
		return r
	}

	search := detailed(row("14:02:11", "Claude", "work", "mail.search", "ok", false, false),
		"read", "12 results", auditRow{})
	read := detailed(row("14:02:09", "Claude", "work", "mail.get_message", "ok", false, false),
		"read", "1 message", auditRow{Targets: []string{"acct_1:1234567890abcde1"}})
	sent := detailed(row("14:01:30", "Claude", "work", "mail.send", "ok", false, false),
		"send", "3 recipients", auditRow{
			To:      []string{"priya@example.com", "sam@partner.example"},
			Cc:      []string{"ada.augusta.byron.lovelace+quarterly-board-reporting@very-long-department-name.example.com"},
			Subject: "Re: quarterly numbers — revised deck attached",
			Targets: []string{"acct_1:1234567890abcde2"},
		})
	refusedSend := detailed(row("13:58:44", "Claude", "personal", "mail.send", "scope_denied", true, false),
		"send", "", auditRow{
			Reason: `scope_denied: this grant holds read, draft, labels on personal - ` +
				`ada@fastmail.example. That action requires "send".`,
		})
	modified := detailed(row("13:40:18", "Claude", "work", "mail.modify", "ok", false, false),
		"labels", "42 messages", auditRow{
			Action:  "+Label_receipts -INBOX +read",
			Targets: []string{"acct_1:1234567890abcde1", "acct_1:1234567890abcde3"},
			More:    40,
		})
	edited := detailed(row("11:20:03", "Claude", "", "grant.edit", "mailbox added", false, true),
		"", "", auditRow{Action: "add", Name: "ada+news@example.com"})
	digest := detailed(row("09:00:00", "Nightly digest", "newsletters", "mail.search", "ok", false, false),
		"read", "0 results", auditRow{})

	brokenProvider := detailed(row("22:14:57", "Importer for the quarterly board reporting pipeline",
		"archive", "mail.get_attachment", "provider_error", true, false),
		"attachments", "", auditRow{
			Action:  "link",
			Targets: []string{"acct_6:1049"},
			Reason:  "provider_error: fetch on archive - archive@example.com (imap): connection reset by peer",
		})
	lapsedGrant := detailed(row("22:14:55", "", "archive", "mail.get_message", "grant_expired", true, false),
		"read", "", auditRow{Reason: "grant has expired"})
	// A row from before the detail columns existed. Nothing backfills them, so this is what
	// every row already in an upgraded database looks like.
	old := row("08:31:02", "Claude", "work", "mail.labels", "ok", false, false)
	old.Undetailed = true

	days := []auditDay{
		{Label: "Today", Rows: []auditRow{search, read, sent, refusedSend, modified, edited, digest}},
		{Label: "Yesterday", Rows: []auditRow{brokenProvider, lapsedGrant, old}},
		{Label: now.Add(-3 * day).Format("Monday 2 January 2006"), Rows: []auditRow{
			detailed(row("17:45:12", "Zapier", "work", "mail.search", "grant_revoked", true, false),
				"read", "", auditRow{Reason: "grant has been revoked"}),
		}},
	}
	page("audit-empty", "audit", "Audit", "audit", "/audit", me, map[string]any{
		"Days": nil, "Refusals": 0, "Total": 0, "Window": auditWindow, "OnlyRefused": false})
	page("audit", "audit", "Audit", "audit", "/audit", me, map[string]any{
		"Days": days, "Refusals": 4, "Total": 11, "Window": auditWindow, "OnlyRefused": false})
	refusedOnly := []auditDay{
		{Label: "Today", Rows: []auditRow{refusedSend}},
		{Label: "Yesterday", Rows: []auditRow{brokenProvider, lapsedGrant}},
	}
	page("audit-refused", "audit", "Audit", "audit", "/audit", me, map[string]any{
		"Days": refusedOnly, "Refusals": 4, "Total": 11, "Window": auditWindow, "OnlyRefused": true})
	page("audit-refused-none", "audit", "Audit", "audit", "/audit", me, map[string]any{
		"Days": nil, "Refusals": 0, "Total": 11, "Window": auditWindow, "OnlyRefused": true})

	// The same page with every row opened. <details> opens without script, so this is a real
	// state a reader reaches by clicking rather than a mock-up — and it is the one the design
	// has to survive, because what is disclosed is where all the new detail is. Derived from
	// the rendered page for the same reason consent-nojs is: the handler has no reason to
	// force these open, so there is no data to render it from.
	if b, err := os.ReadFile(filepath.Join(out, "audit.html")); err == nil {
		opened := strings.ReplaceAll(string(b), "<details>", "<details open>")
		if err := os.WriteFile(filepath.Join(out, "audit-open.html"), []byte(opened), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// --- invites ----------------------------------------------------------------------
	type inviteRow struct {
		ID        string
		Note      string
		CreatedAt time.Time
		ExpiresAt *time.Time
		State     string
	}
	invites := []inviteRow{
		{ID: "inv_1", Note: "Priya, new laptop", CreatedAt: now.Add(-2 * day), ExpiresAt: in(5 * day), State: "open"},
		{ID: "inv_2", Note: "", CreatedAt: now.Add(-9 * day), ExpiresAt: nil, State: "open"},
		{ID: "inv_3", Note: "Contractor account for the Q3 migration work", CreatedAt: now.Add(-20 * day), ExpiresAt: ago(6 * day), State: "expired"},
		{ID: "inv_4", Note: "Sam", CreatedAt: now.Add(-30 * day), ExpiresAt: ago(23 * day), State: "redeemed"},
		{ID: "inv_5", Note: "Issued by mistake", CreatedAt: now.Add(-31 * day), ExpiresAt: in(day), State: "revoked"},
	}
	invitePolicy := signup.Policy{Mode: signup.Invite}
	page("invites-empty", "invites", "Invites", "invites", "/invites", me, map[string]any{
		"Invites": nil, "Policy": invitePolicy.Mode, "Explain": invitePolicy.Describe(), "InInvite": true})
	page("invites", "invites", "Invites", "invites", "/invites", me, map[string]any{
		"Invites": invites, "Policy": invitePolicy.Mode, "Explain": invitePolicy.Describe(), "InInvite": true})
	page("invites-fresh", "invites", "Invites", "invites", "/invites", me, map[string]any{
		"Invites": invites, "Policy": invitePolicy.Mode, "Explain": invitePolicy.Describe(), "InInvite": true,
		"NewCode": "K7QW9ZC2M4N8P1RTVX3H5J6D0BFG",
		"NewLink": "https://mail.example.com/invite/K7QW9ZC2M4N8P1RTVX3H5J6D0BFG"})
	openPolicy := signup.Policy{Mode: signup.Open}
	page("invites-inert", "invites", "Invites", "invites", "/invites", me, map[string]any{
		"Invites": invites, "Policy": openPolicy.Mode, "Explain": openPolicy.Describe(), "InInvite": false})

	// --- consent ----------------------------------------------------------------------
	consentAccounts := func(list []mail.Account, checked map[string]bool) []accountView {
		var out []accountView
		for _, a := range list {
			out = append(out, accountView{ID: a.ID, Alias: a.Alias, Address: a.Address,
				Checked: checked[string(a.ID)]})
		}
		return out
	}
	consentCaps := func(checked map[string]bool, requested map[string]bool) []capView {
		var out []capView
		for _, c := range mail.AllCapabilities {
			v := capViewOf(c)
			v.Checked, v.Requested = checked[string(c)], requested[string(c)]
			out = append(out, v)
		}
		return out
	}
	type req struct {
		RequestID, ClientName, ClientID, Label string
		Redirect                               oauthsrv.RedirectTarget
	}
	// The three destinations the consent screen words differently, spread across the states
	// below so each one is a page somebody has looked at: a remote host here, a loopback on
	// consent-no-mailboxes, a private scheme on consent-nothing-asked, and on consent-stress
	// the host that is not ASCII — which registration now refuses, and which the screen still
	// has to render for a client that registered before it did.
	remote := oauthsrv.RedirectTarget{Origin: "https://claude.ai", Kind: oauthsrv.RedirectRemote, ASCIIHost: true}
	base := req{RequestID: "req_1", ClientName: "Claude Desktop", ClientID: "client_1", Label: "Claude Desktop",
		Redirect: remote}
	asked := map[string]bool{"read": true, "draft": true, "send": true}
	page("consent", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req": base, "Caps": consentCaps(nil, asked),
		"Accounts": consentAccounts(linked, nil), "Requested": []string{"read", "draft", "send"},
		"RequestedUnknown": false, "Expires": "90",
		"Modes": modeViews(grant.DefaultMode, "")})
	// The split between composing a draft and destroying one, as the operator sees it: the
	// state where `draft` is ticked and `discard` is not is the recommended compose grant,
	// and the two boxes have to read as two decisions rather than as one wrapped twice.
	page("consent-compose", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req":              base,
		"Caps":             consentCaps(map[string]bool{"read": true, "draft": true}, asked),
		"Accounts":         consentAccounts(linked, map[string]bool{"acct_1": true}),
		"Requested":        []string{"read", "draft", "send"},
		"RequestedUnknown": false, "Expires": "90",
		"Modes": modeViews(grant.DefaultMode, "")})
	page("consent-privileged", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req":              base,
		"Caps":             consentCaps(map[string]bool{"read": true, "draft": true, "send": true, "destructive": true}, asked),
		"Accounts":         consentAccounts(linked, map[string]bool{"acct_1": true, "acct_2": true}),
		"Requested":        []string{"read", "draft", "send"},
		"RequestedUnknown": true, "Expires": "365",
		"Modes": modeViews(grant.ModeHold, "")})
	page("consent-no-mailboxes", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req": req{RequestID: "req_1", ClientName: "Claude Desktop", ClientID: "client_1", Label: "Claude Desktop",
			Redirect: oauthsrv.RedirectTarget{Origin: "http://127.0.0.1:33418", Kind: oauthsrv.RedirectLoopback, ASCIIHost: true}},
		"Caps": consentCaps(nil, asked), "Accounts": []accountView{},
		"Requested": []string{"read", "draft", "send"}, "RequestedUnknown": false, "Expires": "90",
		"Modes": modeViews(grant.DefaultMode, "")})
	page("consent-nothing-asked", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req": req{RequestID: "req_2", ClientName: "mcp-client-0.1.4 (unregistered build, name supplied at registration)",
			ClientID: "client_2", Label: "",
			Redirect: oauthsrv.RedirectTarget{Origin: "cursor:", Kind: oauthsrv.RedirectScheme, ASCIIHost: true}},
		"Caps": consentCaps(nil, nil), "Accounts": consentAccounts(linked, nil),
		"Requested": []string{}, "RequestedUnknown": true, "Expires": "30",
		"Modes": modeViews(grant.DefaultMode, "")})
	page("consent-stress", "consent", "Authorize", "", "/authorize", me, map[string]any{
		"Req": req{RequestID: "req_3", ClientName: "Quarterly Board Reporting And Investor Update Assistant",
			ClientID: "client_3", Label: "Quarterly Board Reporting And Investor Update Assistant",
			Redirect: oauthsrv.RedirectTarget{Origin: "https://сlaude-assistant-callbacks.example:8443",
				Kind: oauthsrv.RedirectRemote}},
		"Caps":             consentCaps(map[string]bool{"read": true, "attachments": true, "discard": true, "send": true, "settings": true, "destructive": true}, asked),
		"Accounts":         consentAccounts(long, map[string]bool{"acct_4": true}),
		"Requested":        []string{"read", "attachments", "draft", "discard", "send", "labels", "filters", "settings", "destructive"},
		"RequestedUnknown": false, "Expires": "never",
		"Modes": modeViews(grant.ModeUnattended, "")})

	// The consent screen as a browser with the script blocked gets it: the running summary
	// above Approve is the one thing on this UI that cannot exist without script, and it is
	// rendered hidden so that a page which never gets the script is the page without it.
	if b, err := os.ReadFile(filepath.Join(out, "consent-privileged.html")); err == nil {
		html := regexp.MustCompile(`<script[^>]*></script>`).ReplaceAllString(string(b), "")
		if err := os.WriteFile(filepath.Join(out, "consent-nojs.html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// --- README -----------------------------------------------------------------------
	//
	// Two states rendered for the pictures in README.md rather than for this review, so that
	// what a reader is shown comes out of the same render path as everything else and cannot
	// drift from it. They differ from the states above in one way each, and both differences
	// are about not implying more than is true.
	//
	// The mailboxes are linked through Gmail and IMAP only. Zoho and Microsoft are
	// implemented and the page offers them, which is why they are still in the list below —
	// but neither has ever run against a live mailbox, and a picture of a mailbox linked
	// through one, sitting under a green `linked` badge, would say otherwise.
	//
	// The grants are the same clients with the same scope, minus the two mailboxes no other
	// picture has, so that a reader following the story from one image to the next finds the
	// same three mailboxes in both.
	readmeAccounts := []mail.Account{
		acct("acct_1", "work", "ada.lovelace@example.com", mail.ProviderGmail, mail.StatusLinked, now.Add(-3*time.Hour)),
		acct("acct_2", "personal", "ada@fastmail.example", mail.ProviderIMAP, mail.StatusNeedsReauth, now.Add(-9*day)),
		acct("acct_3", "newsletters", "ada+news@example.com", mail.ProviderIMAP, mail.StatusLinked, time.Time{}),
	}
	shotAccounts("readme-accounts", readmeAccounts, nil)

	readmeClaude := claudeA
	readmeClaude.Ambiguous = false
	readmeNightly := nightly
	readmeNightly.Accounts = []string{"work", "newsletters"}
	page("readme-grants", "grants", "Grants", "grants", "/grants", me, map[string]any{
		"Live": []grantView{readmeClaude, readmeNightly}, "Lapsed": nil,
		"Revoked": []grantView{dead}, "SharedLabels": 0, "RevokedOpen": true})

	names, _ := filepath.Glob(filepath.Join(out, "*.html"))
	fmt.Printf("wrote %d pages to %s\n", len(names), out)
}

// loginRefusal is the sentence the sign-in page shows when an identity provider returns the
// given error code, obtained the way the handler obtains it: a real callback into a real
// *auth.OIDC, and auth.LoginMessage on the error that comes back. The callback route in
// web.go makes that same call on that same value, so this fixture cannot outlive the wording
// it depicts — a code dropped from the allowlist, or a sentence reworded, changes the picture
// on the next run instead of leaving it saying something the product no longer says.
//
// A description is sent along because a real issuer sends one, and it is the issuer's words
// rather than ours: it has to be dropped rather than escaped, and a render carrying any of it
// would be a picture of the defect.
func loginRefusal(t *testing.T, code string) string {
	t.Helper()

	provider, err := auth.NewOIDC(t.Context(), auth.OIDCOptions{
		ID: "okta", Label: "Okta", Issuer: fakeIssuer(t), ClientID: "mailroom",
		RedirectURL: "https://mail.example.com/auth/okta/callback",
		Sessions:    auth.NewSessions(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The outbound leg first, so the state coming back is one this provider really minted.
	// Callback checks the state before it reads the error, and one it does not know produces
	// the stale-link message rather than the refusal.
	rec := httptest.NewRecorder()
	provider.StartLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/okta/start", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("starting a login answered %d, want 303: %s", rec.Code, rec.Body.String())
	}
	to, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	back := httptest.NewRequest(http.MethodGet, "/auth/okta/callback?"+url.Values{
		"state":             {to.Query().Get("state")},
		"error":             {code},
		"error_description": {"The user denied the request"},
	}.Encode(), nil)
	_, err = provider.Callback(httptest.NewRecorder(), back)
	if err == nil {
		t.Fatalf("a callback carrying error=%s was accepted", code)
	}
	return auth.LoginMessage(err)
}
