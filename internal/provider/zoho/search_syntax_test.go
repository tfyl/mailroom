package zoho

import (
	"testing"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The bug this pins: free text was sent as a bare word with no field, and the other terms
// used a three-part `field:contains:value` joined by `&&`. Zoho parses neither, so a search
// returned success and nothing — the worst shape a failure can take, because the caller
// concludes the mailbox is empty rather than that the query was malformed.
func TestSearchExpressionUsesZohosOwnSyntax(t *testing.T) {
	cases := []struct {
		name string
		q    mmail.Query
		want string
	}{
		{
			name: "free text searches the whole message",
			q:    mmail.Query{Raw: "accountant"},
			want: "entire:accountant",
		},
		{
			name: "sender",
			q:    mmail.Query{From: "books@example.com"},
			want: "sender:books@example.com",
		},
		{
			name: "recipient",
			q:    mmail.Query{To: "me@example.com"},
			want: "to:me@example.com",
		},
		{
			name: "subject",
			q:    mmail.Query{Subject: "invoice"},
			want: "subject:invoice",
		},
		{
			name: "dates are days, not epochs",
			q: mmail.Query{
				After:  time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC),
				Before: time.Date(2026, 4, 2, 18, 0, 0, 0, time.UTC),
			},
			want: "fromDate:01-Mar-2026::toDate:02-Apr-2026",
		},
		{
			name: "several terms join with ::",
			q:    mmail.Query{Raw: "accountant", From: "books@example.com", Subject: "invoice"},
			want: "entire:accountant::sender:books@example.com::subject:invoice",
		},
		{
			name: "an empty query asks for nothing",
			q:    mmail.Query{},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := searchExpression(c.q); got != c.want {
				t.Errorf("searchExpression()\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// Guards against the shape rather than a particular value: nothing Zoho does not parse
// should be able to creep back in.
func TestSearchExpressionNeverEmitsTheSyntaxZohoRejects(t *testing.T) {
	got := searchExpression(mmail.Query{
		Raw: "accountant", From: "a@example.com", To: "b@example.com", Subject: "invoice",
		After: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	for _, bad := range []string{"&&", ":contains:", "receivedTime"} {
		if contains(got, bad) {
			t.Errorf("expression still contains %q: %s", bad, got)
		}
	}
	if !contains(got, "entire:") {
		t.Errorf("free text must be scoped to a field: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
