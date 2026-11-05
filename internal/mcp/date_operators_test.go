package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

// recordingMailbox remembers the query it was asked, and answers with fixed mail.
type recordingMailbox struct {
	messages []mail.Message
	asked    *mail.Query
}

func (recordingMailbox) ID() mail.ProviderID      { return mail.ProviderIMAP }
func (r recordingMailbox) Capabilities() mail.Set { return mail.DerivedCapabilities(r) }
func (recordingMailbox) Quirks() []mail.Quirk     { return nil }

func (r recordingMailbox) Search(_ context.Context, q mail.Query, _ string) (mail.Page[mail.Message], error) {
	*r.asked = q
	return mail.Page[mail.Message]{Items: r.messages}, nil
}

func (recordingMailbox) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{ID: id}, nil
}

// "assessment after:2026-11-01" reached Zoho as the literal phrase, and came back an empty
// success. The provider must instead be handed the keyword and a structured bound, and mail
// outside the bound must not survive even if a provider's own bound was a whole day wide.
func TestSearchLiftsDateOperatorsOutOfTheQuery(t *testing.T) {
	var asked mail.Query
	tools := fanoutTools(byAccount{
		workMailbox.ID: recordingMailbox{asked: &asked, messages: []mail.Message{
			message(workMailbox.ID, "work", "in", time.Date(2026, 11, 4, 22, 15, 0, 0, time.UTC)),
			message(workMailbox.ID, "work", "out", time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)),
		}},
	})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil,
		searchArgs{Account: "work", Query: "assessment after:2026-10-20T07:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("refused: %v", payload(t, res))
	}
	if asked.Raw != "assessment" {
		t.Errorf("the provider was handed %q; the date operator must not travel as text", asked.Raw)
	}
	if !asked.After.Equal(time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("After = %v, want the full time the caller gave", asked.After)
	}

	body := payload(t, res)
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("only the 4 November message is inside the bound; got %d results", len(results))
	}
	block, ok := body["query"].(map[string]any)
	if !ok {
		t.Fatal("the result must say how the date operator was read")
	}
	if !strings.Contains(strings.Join(toStrings(block["dates"]), " "), "2026-10-20T07:00:00Z") {
		t.Errorf("the query block does not name the bound: %v", block)
	}
}

// A date the server cannot read is refused out loud. Passing it through as text was the
// original bug, in which the answer was zero results reported as success.
func TestSearchRefusesADateItCannotRead(t *testing.T) {
	var asked mail.Query
	tools := fanoutTools(byAccount{workMailbox.ID: recordingMailbox{asked: &asked}})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil,
		searchArgs{Account: "work", Query: "assessment after:last-tuesday"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("an unreadable date must be an error, got %v", payload(t, res))
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, i := range items {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}
