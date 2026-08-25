package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

func postRename(t *testing.T, s *Server, who user.User, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/accounts/rename", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), who))

	rec := httptest.NewRecorder()
	s.rename(rec, r)
	return rec
}

// renamable sets up a signed-in user with one mailbox, which is what every case below needs.
func renamable(t *testing.T, alias string) (*Server, *store.Store, user.User, mail.Account) {
	t.Helper()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")

	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	acct := mail.Account{
		ID: mail.AccountID("acct_" + alias), Alias: alias, Address: alias + "@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(context.Background(), me.ID, acct, "sealed", "read"); err != nil {
		t.Fatal(err)
	}
	return s, db, me, acct
}

func TestRenamingAMailboxFromTheForm(t *testing.T) {
	s, db, me, acct := renamable(t, "work")

	rec := postRename(t, s, me, url.Values{"id": {string(acct.ID)}, "alias": {"personal"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/accounts?renamed=personal" {
		t.Fatalf("Location = %q", got)
	}

	stored, err := db.Account(context.Background(), me.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Alias != "personal" {
		t.Fatalf("alias is %q, want \"personal\"", stored.Alias)
	}
}

// The pattern attribute on the form is advisory; the handler is what has to hold.
func TestRenamingRefusesAnAliasTheFormWouldNotHaveSent(t *testing.T) {
	s, db, me, acct := renamable(t, "work")

	rec := postRename(t, s, me, url.Values{"id": {string(acct.ID)}, "alias": {"not a valid alias"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "letters, numbers, dashes and underscores") {
		t.Fatalf("the page does not say what is wrong: %s", rec.Body.String())
	}

	stored, err := db.Account(context.Background(), me.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Alias != "work" {
		t.Fatalf("the refused rename went through anyway: %q", stored.Alias)
	}
}

// A collision re-renders the page rather than replacing it with a bare error, because the
// mailbox list and the name just typed are both still wanted.
func TestRenamingIntoATakenAliasRerendersThePage(t *testing.T) {
	s, db, me, acct := renamable(t, "work")

	second := mail.Account{
		ID: "acct_second", Alias: "personal", Address: "personal@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(context.Background(), me.ID, second, "sealed", "read"); err != nil {
		t.Fatal(err)
	}

	rec := postRename(t, s, me, url.Values{"id": {string(acct.ID)}, "alias": {"personal"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "already in use") {
		t.Fatalf("the page does not explain the collision: %s", body)
	}
	if !strings.Contains(body, "work@example.com") {
		t.Fatalf("the mailbox list was lost on the error page: %s", body)
	}
}

// Same wording as unlink, and for the same reason: whether an id exists is not something to
// confirm to somebody who does not own it.
func TestRenamingSomebodyElsesMailboxReportsNoSuchMailbox(t *testing.T) {
	s, db, _, acct := renamable(t, "work")

	signInAs(s, "bob", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var bob user.User
	for _, u := range users {
		if u.Subject == "bob" {
			bob = u
		}
	}
	if bob.ID == "" {
		t.Fatal("bob did not get a user row")
	}

	rec := postRename(t, s, bob, url.Values{"id": {string(acct.ID)}, "alias": {"stolen"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such mailbox") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
