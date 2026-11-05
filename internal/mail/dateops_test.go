package mail

import (
	"strings"
	"testing"
	"time"
)

func TestExtractDateOperators(t *testing.T) {
	now := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	utc := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }

	cases := []struct {
		raw           string
		text          string
		after, before time.Time
	}{
		// The live failures that started this.
		{"assessment after:2026-11-01", "assessment", utc(2026, 11, 1, 0), time.Time{}},
		{"selfassessment after:2026-10-20", "selfassessment", utc(2026, 10, 20, 0), time.Time{}},
		{"after:2026-10-20T07:00:00Z", "", utc(2026, 10, 20, 7), time.Time{}},
		// Gmail's own spellings.
		{"invoice after:2026/10/01 before:2026/11/01", "invoice", utc(2026, 10, 1, 0), utc(2026, 11, 1, 0)},
		{"older:2026/1/2 from:x", "from:x", time.Time{}, utc(2026, 1, 2, 0)},
		{"newer_than:7d", "", now.AddDate(0, 0, -7), time.Time{}},
		{"older_than:1y", "", time.Time{}, now.AddDate(-1, 0, 0)},
		{"after:1792454400", "", time.Unix(1792454400, 0).UTC(), time.Time{}},
		{"after:2026-10-20T07:00", "", utc(2026, 10, 20, 7), time.Time{}},
		{"after:2026-10-20T08:00:00+01:00", "", utc(2026, 10, 20, 7), time.Time{}},
		// The tighter of two bounds wins.
		{"after:2026-01-01 after:2026-02-01", "", utc(2026, 2, 1, 0), time.Time{}},
		// Quoted text is the caller's phrase, not an operator.
		{`"meet after:lunch" x`, `"meet after:lunch" x`, time.Time{}, time.Time{}},
		{"no operators here", "no operators here", time.Time{}, time.Time{}},
		{"AFTER:2026-11-01 x", "x", utc(2026, 11, 1, 0), time.Time{}},
	}
	for _, c := range cases {
		got, err := ExtractDateOperators(c.raw, now)
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if got.Text != c.text || !got.After.Equal(c.after) || !got.Before.Equal(c.before) {
			t.Errorf("%q: got text=%q after=%v before=%v; want text=%q after=%v before=%v",
				c.raw, got.Text, got.After, got.Before, c.text, c.after, c.before)
		}
	}
}

// An unreadable date is refused. Passing it through as text is what produced the empty
// successes, so it must never be the fallback.
func TestUnreadableDateIsRefused(t *testing.T) {
	for _, raw := range []string{"after:yesterday", "before:2026-13-01", "newer_than:3w", "after:"} {
		_, err := ExtractDateOperators(raw, time.Now())
		if err == nil || !strings.Contains(err.Error(), "not a date") {
			t.Errorf("%q: want a refusal naming the date, got %v", raw, err)
		}
	}
}

func TestSearchWindowStopsPastAfterAndWalksToFindMatches(t *testing.T) {
	d := func(day int) Message { return Message{Date: time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC)} }
	pages := map[string]Page[Message]{
		"":  {Items: []Message{d(20), d(19)}, Cursor: "2"}, // all newer than Before
		"2": {Items: []Message{d(10), d(9)}, Cursor: "3"},
		"3": {Items: []Message{d(5), d(1)}, Cursor: "4"}, // runs past After (minus slack)
	}
	var calls int
	q := Query{After: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), Before: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}
	page, err := SearchWindow(q, "", func(c string) (Page[Message], error) {
		calls++
		return pages[c], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(page.Items) != 2 || page.Cursor != "3" {
		t.Fatalf("want the two 9–10 Nov messages after skipping a page of newer mail, cursor 3; got %d calls, %d items, cursor %q",
			calls, len(page.Items), page.Cursor)
	}
}
