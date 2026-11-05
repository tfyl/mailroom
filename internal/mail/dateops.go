package mail

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DateOperators is what ExtractDateOperators took out of a free-text query.
type DateOperators struct {
	// Text is the query with every date operator removed, ready to hand to a provider.
	Text string
	// After is inclusive and Before exclusive, the same as Query's fields. Zero when absent.
	After  time.Time
	Before time.Time
	// Read lists each operator as it was understood, for the caller to check.
	Read []string
}

// ExtractDateOperators lifts Gmail-style date operators out of a free-text query so they can
// travel as structured bounds instead of as text.
//
// The reason this exists is that the text is otherwise handed to whichever provider the
// mailbox lives on, and only one of them speaks Gmail's syntax. Zoho wrapped the whole query
// in quotes and searched for the literal phrase "assessment after:2026-11-01"; Outlook's KQL
// has no after: property; and Gmail itself reads after:2026-10-20T07:00:00Z as nothing at all.
// Every one of those answered success with zero results, which a caller reads as "no such
// mail". As structured bounds, each provider renders the date in its own syntax, or the
// gateway filters on it, and the same query means the same thing on every mailbox.
//
// Operators inside double quotes are left alone, since a quoted phrase is text the caller
// wants matched. A value that cannot be read is an error rather than something passed
// through, because passing it through is exactly what made the empty answers.
//
// Accepted: after:, before:, newer:, older: with YYYY-MM-DD, YYYY/MM/DD, an RFC 3339 time
// (with or without seconds or zone; no zone means UTC) or Unix seconds; newer_than: and
// older_than: with Nd, Nm or Ny relative to now. A bare date is midnight UTC, so after: a
// date includes that day and before: a date excludes it, which is Gmail's reading.
func ExtractDateOperators(raw string, now time.Time) (DateOperators, error) {
	out := DateOperators{}
	var kept []string

	for _, tok := range tokenize(raw) {
		op, value, ok := dateOperator(tok)
		if !ok {
			kept = append(kept, tok)
			continue
		}

		var bound time.Time
		var err error
		switch op {
		case "newer_than", "older_than":
			bound, err = relative(value, now)
		default:
			bound, err = absolute(value)
		}
		if err != nil {
			return DateOperators{}, fmt.Errorf("%s:%s is not a date this server can read (%v); "+
				"use YYYY-MM-DD, an RFC 3339 time such as 2026-10-20T07:00:00Z, Unix seconds, "+
				"or newer_than:/older_than: with Nd, Nm or Ny", op, value, err)
		}

		switch op {
		case "after", "newer", "newer_than":
			if out.After.IsZero() || bound.After(out.After) {
				out.After = bound
			}
		case "before", "older", "older_than":
			if out.Before.IsZero() || bound.Before(out.Before) {
				out.Before = bound
			}
		}
		out.Read = append(out.Read, fmt.Sprintf("%s:%s → %s", op, value, bound.UTC().Format(time.RFC3339)))
	}

	out.Text = strings.Join(kept, " ")
	return out, nil
}

// tokenize splits on whitespace outside double quotes, keeping the quotes on the token.
func tokenize(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			b.WriteRune(r)
		case !quoted && (r == ' ' || r == '\t' || r == '\n'):
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

var dateOps = []string{"newer_than", "older_than", "after", "before", "newer", "older"}

func dateOperator(tok string) (op, value string, ok bool) {
	lower := strings.ToLower(tok)
	for _, op := range dateOps {
		if strings.HasPrefix(lower, op+":") {
			return op, strings.Trim(tok[len(op)+1:], `"`), true
		}
	}
	return "", "", false
}

func absolute(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, fmt.Errorf("empty value")
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && len(v) >= 9 {
		return time.Unix(n, 0).UTC(), nil
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04",
		"2006-01-02",
		"2006/01/02",
		"2006/1/2",
		"2006-1-2",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised format")
}

var relativePattern = regexp.MustCompile(`^(\d+)([dmy])$`)

func relative(v string, now time.Time) (time.Time, error) {
	m := relativePattern.FindStringSubmatch(strings.ToLower(v))
	if m == nil {
		return time.Time{}, fmt.Errorf("want a count followed by d, m or y")
	}
	n, _ := strconv.Atoi(m[1])
	now = now.UTC()
	switch m[2] {
	case "d":
		return now.AddDate(0, 0, -n), nil
	case "m":
		return now.AddDate(0, -n, 0), nil
	default:
		return now.AddDate(-n, 0, 0), nil
	}
}

// InWindow reports whether t falls inside the query's date bounds: on or after After, and
// strictly before Before.
func (q Query) InWindow(t time.Time) bool {
	if !q.After.IsZero() && t.Before(q.After) {
		return false
	}
	if !q.Before.IsZero() && !t.Before(q.Before) {
		return false
	}
	return true
}

// HasWindow reports whether the query carries either date bound.
func (q Query) HasWindow() bool { return !q.After.IsZero() || !q.Before.IsZero() }

// windowSlack is how far past After a newest-first listing has to go before the rest of it
// is assumed to be older. Zoho and Graph search order by a date that is not quite the one
// filtered on (sent rather than received), and Zoho's paging is unstable, so stopping at the
// first older message would occasionally stop one too early.
const windowSlack = 48 * time.Hour

// windowPages bounds how many provider pages one call walks looking for mail inside the
// window, so a Before far in the past cannot turn one search into a crawl of the mailbox.
const windowPages = 8

// SearchWindow serves a query's date bounds in the gateway, for a provider whose own search
// cannot be trusted to: fetch pages without the dates, keep what falls inside the window,
// and stop once a newest-first listing is past After.
//
// fetch is the provider's own search for one page, with the dates already left out of
// whatever it sends. It must return mail newest first, which is what the stop relies on.
func SearchWindow(q Query, cursor string, fetch func(cursor string) (Page[Message], error)) (Page[Message], error) {
	var kept []Message
	for i := 0; i < windowPages; i++ {
		page, err := fetch(cursor)
		if err != nil {
			return Page[Message]{}, err
		}
		pastAfter := false
		for _, m := range page.Items {
			if q.InWindow(m.Date) {
				kept = append(kept, m)
			}
			if !q.After.IsZero() && !m.Date.IsZero() && m.Date.Before(q.After.Add(-windowSlack)) {
				pastAfter = true
			}
		}
		cursor = page.Cursor
		if pastAfter {
			cursor = ""
		}
		if len(kept) > 0 || cursor == "" {
			break
		}
	}
	return Page[Message]{Items: kept, Cursor: cursor}, nil
}
