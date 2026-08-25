package zoho

import (
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// A leading hyphen does not fail on Zoho, it inverts. Asked for a subject beginning with one,
// Zoho answered with the whole mailbox and called it a match — measured live, where a
// nonsense term returned nothing and the same term with a hyphen in front returned every
// message on the page.
func TestSearchTermKeepsATermFromBecomingSyntax(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a plain word is left alone", "invoice", "invoice"},
		{"an internal hyphen is not an operator", "well-known", "well-known"},
		{"an address is left alone", "offers@shop.example", "offers@shop.example"},
		{"a leading hyphen would invert", "-invoice", `"-invoice"`},
		{"whitespace needs quoting", "two words", `"two words"`},
		{"a stray quote is removed", `say"hello`, `"sayhello"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := searchTerm(tc.in); got != tc.want {
				t.Fatalf("searchTerm(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole expression, so the quoting cannot be applied to one field and forgotten on the
// next. Zoho joins conditions with ::, and every field carrying somebody else's text goes
// through the same rendering.
func TestSearchExpressionQuotesEveryTextField(t *testing.T) {
	got := searchExpression(mmail.Query{
		Raw:     "-everything",
		From:    "-sender@example.com",
		To:      "-recipient@example.com",
		Subject: "-subject",
	})

	for _, want := range []string{
		`entire:"-everything"`,
		`sender:"-sender@example.com"`,
		`to:"-recipient@example.com"`,
		`subject:"-subject"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expression %q is missing %q", got, want)
		}
	}
}

// And an ordinary query must be unchanged, because quoting is not free: the point of the
// narrow rule is that searches people already rely on keep matching what they matched.
func TestAnOrdinaryExpressionIsUnquoted(t *testing.T) {
	got := searchExpression(mmail.Query{From: "someone@example.com", Subject: "invoice"})
	if got != "sender:someone@example.com::subject:invoice" {
		t.Fatalf("ordinary query rendered as %q", got)
	}
}
