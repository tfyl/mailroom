package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// Cloudflare rewrites any address it finds in the HTML unless it is wrapped in these
// markers, and this UI cannot run the script that would undo the rewrite. The markers must
// therefore survive rendering — which is the part that is easy to get wrong, because
// html/template drops a comment written literally in a template without complaint.
func TestAddressesCarryTheObfuscationOptOut(t *testing.T) {
	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")

	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	me := users[0]

	acct := mail.Account{
		ID: "acct_one", Alias: "work", Address: "operator@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(context.Background(), me.ID, acct, "sealed", "read"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	r = r.WithContext(user.NewContext(r.Context(), me))
	rec := httptest.NewRecorder()
	s.accounts(rec, r)

	body := rec.Body.String()
	want := "<!--email_off-->operator@example.com<!--/email_off-->"
	if !strings.Contains(body, want) {
		t.Fatalf("the mailbox address is not wrapped for Cloudflare.\nwant: %s\nbody: %s", want, body)
	}
}

func TestTheOptOutEscapesTheAddress(t *testing.T) {
	got := string(mailAddress(`a"<script>@example.com`))
	if strings.Contains(got, "<script>") {
		t.Fatalf("the address went out unescaped: %s", got)
	}
	if !strings.HasPrefix(got, "<!--email_off-->") || !strings.HasSuffix(got, "<!--/email_off-->") {
		t.Fatalf("markers missing: %s", got)
	}
}
