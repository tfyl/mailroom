package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

// A search hands back the newest matches up to its limit and stops. That makes every capped
// result a claim about a window rather than about the mailbox, and the two readings come
// apart exactly when the thing being looked for is old: "no results after the newest fifty"
// and "no such message" are different sentences, and only one of them is true.
//
// These tests are about the second one never being available to a caller. The date window is
// the part that does the work — a caller that has been told the oldest row it received is
// from January can see for itself that a message from last April was never in scope — so the
// window is asserted on, not merely the presence of a warning.

// pagedMailbox returns a fixed page and always claims another one follows, which is what a
// provider looks like when a search matched more than the page it handed over.
type pagedMailbox struct{ messages []mail.Message }

func (pagedMailbox) ID() mail.ProviderID      { return mail.ProviderGmail }
func (p pagedMailbox) Capabilities() mail.Set { return mail.DerivedCapabilities(p) }
func (pagedMailbox) Quirks() []mail.Quirk     { return nil }

func (p pagedMailbox) Search(context.Context, mail.Query, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{Items: p.messages, Cursor: "more"}, nil
}

func (pagedMailbox) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{ID: id, Account: "work", Date: time.Now()}, nil
}

// The failure this reproduces: a query whose matches span years, answered with a page
// covering only the most recent stretch of them, read as though it had covered all of it.
func TestTruncatedSearchNamesTheWindowItCovered(t *testing.T) {
	newest := time.Date(2026, 8, 28, 14, 4, 27, 0, time.UTC)
	oldest := time.Date(2026, 1, 17, 19, 6, 16, 0, time.UTC)

	tools := fanoutTools(byAccount{
		workMailbox.ID: pagedMailbox{messages: []mail.Message{
			message(workMailbox.ID, "work", "m1", newest),
			message(workMailbox.ID, "work", "m2", oldest),
		}},
	})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil, searchArgs{Account: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the search was refused: %v", payload(t, res))
	}

	body := payload(t, res)
	if body["complete"] != false {
		t.Fatalf("a provider reporting a further page must leave the result incomplete, got %v",
			body["complete"])
	}

	block, ok := body["truncated"].(map[string]any)
	if !ok {
		t.Fatal("an incomplete search returned no truncated block, so nothing told the caller " +
			"that older mail went unexamined")
	}

	// The oldest row received is the edge of what was searched. Getting this wrong would
	// assert that mail was examined when it was not, which is worse than saying nothing.
	if got := block["covers_back_to"]; got != "2026-01-17T19:06:16Z" {
		t.Errorf("covers_back_to must name the oldest row returned, got %v", got)
	}
	if got := block["covers_up_to"]; got != "2026-08-28T14:04:27Z" {
		t.Errorf("covers_up_to must name the newest row returned, got %v", got)
	}

	// The note is the part a model reads. It has to name the consequence, not just the fact
	// of there being more, and it has to say what to do instead.
	note, _ := block["note"].(string)
	for _, want := range []string{"covers_back_to", "not examined", "cursor"} {
		if !strings.Contains(note, want) {
			t.Errorf("the truncation note must mention %q, got %q", want, note)
		}
	}
}

// A result that reached the end has examined everything the query could match, so there is no
// window to warn about and a block here would teach a caller to ignore the ones that matter.
func TestCompleteSearchCarriesNoTruncationBlock(t *testing.T) {
	tools := fanoutTools(byAccount{
		workMailbox.ID: searchable{messages: []mail.Message{
			message(workMailbox.ID, "work", "m1", time.Now()),
		}},
	})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil, searchArgs{Account: "work"})
	if err != nil {
		t.Fatal(err)
	}

	body := payload(t, res)
	if body["complete"] != true {
		t.Fatalf("an exhausted mailbox should complete, got %v", body["complete"])
	}
	if _, present := body["truncated"]; present {
		t.Error("a complete search must not warn about a window it did not stop short of")
	}
}

// An incomplete result with no rows was cut short by a mailbox that failed rather than by the
// limit, and the per-account status block already carries that. A window spanning no messages
// would be invented, and inventing one here would put dates on a search that returned nothing.
func TestTruncationDescribesNoWindowWithoutMessages(t *testing.T) {
	if got := truncation(nil); got != nil {
		t.Errorf("no messages means no window to describe, got %v", got)
	}
}

// The window is computed rather than read off the ends of the slice, so that an ordering
// change upstream cannot silently turn it into a wrong claim about what was searched.
func TestTruncationWindowDoesNotAssumeOrdering(t *testing.T) {
	early := time.Date(2025, 4, 3, 22, 34, 37, 0, time.UTC)
	late := time.Date(2026, 8, 13, 19, 46, 22, 0, time.UTC)

	got := truncation([]mail.Message{
		message(workMailbox.ID, "work", "m1", early),
		message(workMailbox.ID, "work", "m2", late),
		message(workMailbox.ID, "work", "m3", early.Add(time.Hour)),
	})

	if got["covers_back_to"] != "2025-04-03T22:34:37Z" {
		t.Errorf("covers_back_to must be the earliest date present, got %v", got["covers_back_to"])
	}
	if got["covers_up_to"] != "2026-08-13T19:46:22Z" {
		t.Errorf("covers_up_to must be the latest date present, got %v", got["covers_up_to"])
	}
}
