package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/signup"
)

// tagFor returns the one element whose opening tag carries this id, so a test can assert
// about the attributes on it rather than about the whole page.
func tagFor(t *testing.T, body, id string) string {
	t.Helper()

	at := strings.Index(body, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("no element with id %q on the page: %s", id, body)
	}
	open := strings.LastIndex(body[:at], "<")
	end := strings.Index(body[at:], ">")
	if open < 0 || end < 0 {
		t.Fatalf("could not read the tag around id %q", id)
	}
	return body[open : at+end+1]
}

// Unlinking destroys a stored credential and drops every grant naming the mailbox, and it is
// deliberately not asked about on a page of its own — the mailbox is relinked by the person
// looking at the screen. That reasoning survives only while the button is hard to press by
// accident, and a required checkbox is what the browser will enforce with no script.
func TestUnlinkingCannotBePressedWithoutTicking(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	body := renderAccountsPage(t, s, me)
	tick := tagFor(t, body, "unlink-acct_1")
	if !strings.Contains(tick, "required") {
		t.Errorf("the unlink confirmation can be skipped, so the button is one stray click: %s", tick)
	}
	if !strings.Contains(tick, `type="checkbox"`) {
		t.Errorf("the unlink confirmation is not a checkbox: %s", tick)
	}
	// And it is not sitting in the row: reaching it takes opening the disclosure first.
	if !strings.Contains(body, `<summary>Rename or unlink work</summary>`) {
		t.Errorf("the row's controls are not behind a disclosure: %s", body)
	}
}

// A rejected rename used to redraw the page with a banner at the top and the list below it
// reset, leaving the reader to work out which of their mailboxes it was about and to type the
// name again. It now comes back on the row it names, open, with the name still in the field.
func TestARejectedRenameComesBackOnItsOwnRow(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	rec := postRename(t, s, me, url.Values{"id": {"acct_1"}, "alias": {"not a valid alias"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unusable alias, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `value="not a valid alias"`) {
		t.Errorf("the rejected name was dropped instead of coming back to be corrected: %s", body)
	}
	if !strings.Contains(body, `class="advanced mailbox-manage" open>`) {
		t.Errorf("the row holding the field being corrected is still closed: %s", body)
	}
	if strings.Contains(body, "<strong>Not renamed.</strong>") {
		t.Errorf("the error is drawn twice: once on the row and once at the top of the page: %s", body)
	}
	if !strings.Contains(tagFor(t, body, "alias-acct_1"), `aria-invalid="true"`) {
		t.Errorf("the field that was rejected is not marked as such: %s", body)
	}
}

// The same for the linking form: an error that belongs to one input is drawn under that
// input. A banner above a nine-field form says something is wrong and not which thing.
func TestARejectedIMAPFieldIsAnsweredUnderThatField(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)

	rec := postLinkIMAP(t, s, me, url.Values{
		"alias": {"second"}, "address": {"ada@example.com"}, "password": {"hunter2"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a missing IMAP server, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(tagFor(t, body, "imap-host"), `aria-invalid="true"`) {
		t.Errorf("the field the error is about is not marked as such: %s", body)
	}
	if strings.Contains(body, "<strong>Not linked.</strong>") {
		t.Errorf("a field's own error is also being drawn as a form-wide banner: %s", body)
	}
	// And the disclosure holding the form is open, or the error is behind a closed summary.
	if !strings.Contains(body, `<details name="provider" open>`) {
		t.Errorf("the linking form the error belongs to is closed: %s", body)
	}
}

// Four providers presented as a choice: somebody who uses one of them should not scroll past
// three they do not. Nothing is open for somebody who came to look at the mailboxes they have,
// and the one provider a bare instance can actually use is open for somebody who has none.
func TestTheProviderChooserOpensOnlyWhatIsUseful(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})

	signInAs(s, "ada", "")
	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	first := renderAccountsPage(t, s, users[0])
	if n := strings.Count(first, `<details name="provider" open>`); n != 1 {
		t.Errorf("a first run should open exactly one provider, got %d: %s", n, first)
	}
	// With no OAuth client configured, IMAP is the only one that can work at all, so it is
	// the one whose summary follows the open tag.
	at := strings.Index(first, `<details name="provider" open>`)
	if summary := first[at:min(at+300, len(first))]; !strings.Contains(summary, "Any IMAP server") {
		t.Errorf("the open provider is not the one this instance can use: %s", summary)
	}

	me, _ := aGrant(t, s, db)
	with := renderAccountsPage(t, s, me)
	if strings.Contains(with, `<details name="provider" open>`) {
		t.Errorf("a page with mailboxes on it should open no linking form: %s", with)
	}
	// Mutually exclusive without any script: the browser closes the others.
	if n := strings.Count(with, `<details name="provider"`); n != 4 {
		t.Errorf("want four providers sharing one name, got %d: %s", n, with)
	}
}

// Every input the pages ask a person to fill in has a label pointing at it. Cheap to keep and
// easy to lose, since a field renders perfectly well with no label at all.
func TestEveryFieldOnTheMailboxesPageIsLabelled(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	me, _ := aGrant(t, s, db)
	body := renderAccountsPage(t, s, me)

	for _, id := range []string{
		"google-alias", "microsoft-alias", "zoho-alias",
		"imap-alias", "imap-address", "imap-host", "imap-password",
		"imap-smtp", "imap-username", "imap-smtp-from", "imap-insecure",
		"alias-acct_1", "unlink-acct_1", "mcp-endpoint",
	} {
		if !strings.Contains(body, `for="`+id+`"`) {
			t.Errorf("%s has no label, so it is unnamed to a screen reader", id)
		}
	}
}

// The sign-in page is the one an unauthenticated visitor sees, and it is one choice: the
// column it is in is what makes it read as a page rather than as content that failed to load.
func TestTheSignInPageIsACentredColumn(t *testing.T) {
	s, _ := testServer(t, signup.Policy{Mode: signup.Open})

	rec := httptest.NewRecorder()
	s.loginForm(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if !strings.Contains(rec.Body.String(), `<div class="signin">`) {
		t.Errorf("the sign-in page is not in its own column: %s", rec.Body)
	}
}
