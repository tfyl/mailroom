package web

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// aGrant puts a usable grant in front of the tests: a client, a mailbox and a grant naming
// it, all belonging to the signed-in user.
func aGrant(t *testing.T, s *Server, db *store.Store) (user.User, grant.ID) {
	t.Helper()
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
	account := mail.Account{
		ID: "acct_1", Alias: "work", Address: "ada@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(ctx, me.ID, account, "sealed", ""); err != nil {
		t.Fatal(err)
	}
	g := &grant.Grant{
		ID: "grant_1", OwnerID: me.ID, ClientID: "client_1", Label: "An agent",
		Accounts: []mail.AccountID{account.ID}, Caps: mail.NewSet(mail.CapRead),
	}
	if err := db.CreateGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	return me, g.ID
}

func postRevoke(s *Server, who user.User, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/grants/revoke", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.revokeGrant(rec, r)
	return rec
}

// Revoking cannot be undone by the person doing it, so it asks — on a page, because the
// content-security policy forbids the script a dialog would take.
func TestRevokingAGrantAsksFirst(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)

	rec := postRevoke(s, me, url.Values{"id": {string(id)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("want the confirmation page, got %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cannot be undone") {
		t.Errorf("the confirmation should say what revoking costs: %s", body)
	}
	if !strings.Contains(body, "An agent") {
		t.Errorf("the confirmation should name the grant it is about: %s", body)
	}

	g, err := db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.Revoked() {
		t.Fatal("asking the question revoked the grant anyway")
	}
}

func TestConfirmedRevocationTakesEffect(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, id := aGrant(t, s, db)

	rec := postRevoke(s, me, url.Values{"id": {string(id)}, "confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after revoking, got %d: %s", rec.Code, rec.Body)
	}

	g, err := db.Grant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Revoked() {
		t.Fatal("the grant survived a confirmed revocation")
	}
}

// Another user's grant is not confirmable either: the question itself would tell them the
// grant exists and what it reaches.
func TestTheConfirmationIsScopedToItsOwner(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	_, id := aGrant(t, s, db)

	signInAs(s, "mallory", "")
	users, _ := db.ListUsers(context.Background())
	var mallory user.User
	for _, u := range users {
		if u.Subject == "mallory" {
			mallory = u
		}
	}

	if rec := postRevoke(s, mallory, url.Values{"id": {string(id)}}); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for somebody else's grant, got %d: %s", rec.Code, rec.Body)
	}
}

// The narrowed successor to the rule that used to say "no script at all".
//
// The UI now has exactly one script, and it is an external file served from this origin under
// `script-src 'self'`. What has not changed is that nothing may be inline: no <script> with a
// body, no on* attribute, no javascript: URL. All three are dead markup under a policy with no
// 'unsafe-inline' — they never run — and dead markup on a consent screen is worse than absent,
// because a button that looks guarded and is not is exactly what this screen exists to prevent.
// Keeping 'unsafe-inline' out is also the whole value of having a script-src at all: an
// injection that reaches the page still cannot execute.
//
// A <script src=…></script> with an empty body is what the layout carries and all that passes.
func TestNoTemplateCarriesInlineScript(t *testing.T) {
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	tag := regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	src := regexp.MustCompile(`(?i)\ssrc\s*=\s*["']`)

	entries, err := fs.Glob(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates found to check")
	}
	for _, name := range entries {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := handler.FindIndex(body); loc != nil {
			t.Errorf("%s carries an inline event handler the policy blocks: %s",
				name, body[loc[0]:min(loc[1]+40, len(body))])
		}
		if strings.Contains(strings.ToLower(string(body)), "javascript:") {
			t.Errorf("%s carries a javascript: URL the policy blocks", name)
		}

		tags := tag.FindAllSubmatch(body, -1)
		// Counted as well as matched: an unclosed or self-closed <script> matches nothing
		// above and would otherwise be checked by nothing at all.
		if opened := strings.Count(strings.ToLower(string(body)), "<script"); opened != len(tags) {
			t.Errorf("%s has %d <script tags and %d complete ones; every script must be "+
				"<script src=…></script>", name, opened, len(tags))
		}
		for _, m := range tags {
			if !src.Match(m[1]) {
				t.Errorf("%s carries a script with no src: only an external file is allowed, "+
					"and only from this origin", name)
			}
			if strings.TrimSpace(string(m[2])) != "" {
				t.Errorf("%s carries a script with a body, which the policy blocks: %s",
					name, m[2])
			}
		}
	}
}

// A mailbox with no SMTP server cannot send, and the consent screen offers `send` anyway —
// it lists every capability rather than what each mailbox implements. The page has to say
// where that is refused, because saying it is prevented here is not true.
func TestTheMailboxPageDoesNotPromiseAFilterThatIsNotThere(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	r := httptest.NewRequest(http.MethodGet, "/accounts?linked=work&sending=off", nil)
	r = r.WithContext(user.NewContext(r.Context(), me))
	rec := httptest.NewRecorder()
	s.accounts(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, "refused as unsupported") {
		t.Errorf("the page should say where a send against this mailbox is refused: %s", body)
	}
	for _, claim := range []string{"can never offer", "is withheld from it"} {
		if strings.Contains(body, claim) {
			t.Errorf("the page still claims the grant is filtered (%q): %s", claim, body)
		}
	}
}
