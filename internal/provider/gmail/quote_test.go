package gmail

import (
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The leading hyphen is the case that matters, because it does not fail — it inverts. Gmail
// reads subject:-foo as "subject does not contain foo", so the search answers with the whole
// mailbox and calls it a match. Measured against a live mailbox: the nonsense term returned
// nothing, and the same term with a hyphen in front returned every message on the page.
func TestQuoteKeepsTermsFromBecomingSyntax(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a plain word is left alone", "invoice", "invoice"},
		{"an internal hyphen is not an operator", "well-known", "well-known"},
		{"an address is left alone", "someone@example.com", "someone@example.com"},
		{"a leading hyphen would negate", "-invoice", `"-invoice"`},
		{"whitespace needs quoting", "two words", `"two words"`},
		{"a stray quote is removed", `say"hello`, `"sayhello"`},
		{"quotes and spaces together", `a "quoted" phrase`, `"a quoted phrase"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quote(tc.in); got != tc.want {
				t.Fatalf("quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole query, not just the helper: a hyphenated subject must not reach Gmail as a
// negation, and an ordinary one must not start being quoted, because quoting changes what
// Gmail matches — word variants on a bare term, an exact phrase on a quoted one.
func TestBuildQueryDoesNotInvertOnAHyphen(t *testing.T) {
	negated := buildQuery(mmail.Query{Subject: "-urgent"})
	if strings.Contains(negated, "subject:-urgent") {
		t.Errorf("a hyphenated subject reached Gmail as a negation: %q", negated)
	}
	if !strings.Contains(negated, `subject:"-urgent"`) {
		t.Errorf("want the term quoted, got %q", negated)
	}

	ordinary := buildQuery(mmail.Query{Subject: "urgent"})
	if !strings.Contains(ordinary, "subject:urgent") {
		t.Errorf("an ordinary subject should stay unquoted, got %q", ordinary)
	}
}
