package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// handleSettings refuses to write forwarding because it "hands somebody else access to the
// mail itself, which is a decision for a person at a settings page". A filter that forwards
// is that same act with a delay and a repeat on it — nobody watches a rule run, it applies to
// mail that has not arrived yet, and Graph does not verify the destination.
//
// Refusing in one tool and accepting in the other left the product's own headline threat —
// forwarding a stranger a copy of everything — reachable through the door marked "rule
// management", under a capability the consent screen calls routine.
func TestAFilterCannotForwardMailAway(t *testing.T) {
	work := &stubFilters{}
	tools := filterTools(work)

	res, _, err := tools.handleFilters(grantOver(mail.CapFilters), nil, filtersArgs{
		Action: "create", Account: "work", From: "bank@example.com",
		Forward: "attacker@evil.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a forwarding filter must be refused: %v", payload(t, res))
	}
	if len(work.created) != 0 {
		t.Fatalf("the filter was created despite the refusal: %+v", work.created)
	}

	message, _ := payload(t, res)["message"].(string)
	if !strings.Contains(message, "attacker@evil.example") {
		t.Errorf("the refusal should name the address it declined to forward to: %s", message)
	}
	if !strings.Contains(message, "mail_settings will not write it either") {
		t.Errorf("the refusal should point at the rule it is being consistent with: %s", message)
	}
}

// Holding every capability does not buy it either: this is a product decision about what an
// agent arranges on somebody's behalf, not a permission level.
func TestEvenAFullyPrivilegedGrantCannotForward(t *testing.T) {
	work := &stubFilters{}
	tools := filterTools(work)

	res, _, err := tools.handleFilters(
		grantOver(mail.CapFilters, mail.CapDestructive, mail.CapSettings, mail.CapSend), nil,
		filtersArgs{Action: "create", Account: "work", From: "bank@example.com",
			Forward: "attacker@evil.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("forwarding is refused whatever the grant holds: %v", payload(t, res))
	}
	if len(work.created) != 0 {
		t.Fatalf("the filter was created: %+v", work.created)
	}
}

// The refusal must not take ordinary rule management with it.
func TestAnOrdinaryFilterIsStillCreated(t *testing.T) {
	work := &stubFilters{}
	tools := filterTools(work)

	res, _, err := tools.handleFilters(grantOver(mail.CapFilters), nil, filtersArgs{
		Action: "create", Account: "work", From: "newsletter@example.com",
		Subject: "weekly",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a filter that files mail needs only `filters`: %v", payload(t, res))
	}
	if len(work.created) != 1 {
		t.Fatalf("the filter should have been created, got %+v", work.created)
	}
	if work.created[0].Forward != "" {
		t.Errorf("no forward should have reached the provider: %q", work.created[0].Forward)
	}
}

// stubFilters is a mailbox that can manage filters and records what actually reached it,
// which is the only way to tell a refusal from a creation that was reported as one.
type stubFilters struct {
	created []mail.Filter
	filters []mail.Filter
}

func (s *stubFilters) ID() mail.ProviderID    { return mail.ProviderGmail }
func (s *stubFilters) Capabilities() mail.Set { return mail.DerivedCapabilities(s) }
func (s *stubFilters) Quirks() []mail.Quirk   { return nil }

func (s *stubFilters) ListFilters(context.Context) ([]mail.Filter, error) { return s.filters, nil }

func (s *stubFilters) CreateFilter(_ context.Context, f mail.Filter) (mail.Filter, error) {
	f.ID = "filter_1"
	s.created = append(s.created, f)
	return f, nil
}

func (s *stubFilters) DeleteFilter(context.Context, string) error { return nil }

func filterTools(work *stubFilters) *Tools {
	return fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubFilters{}})
}
