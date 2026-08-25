package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

// An alias is a label somebody chose. "work" is not a mailbox, and a caller told only "work"
// cannot say which of a person's mailboxes it just read, sent from, or emptied — nor can
// whoever reads the transcript afterwards, which is when it matters, because by then the
// mail has gone. Every result that names a mailbox names its address too.
//
// The address is added beside the alias rather than folded into it. The alias is what a
// caller selects by, so it stays something that can be handed straight back; the pair of
// tests at the bottom is the one that would break if that were forgotten.

// searchable is a mailbox holding fixed messages, so a fan-out has something to merge.
type searchable struct{ messages []mail.Message }

func (searchable) ID() mail.ProviderID      { return mail.ProviderGmail }
func (s searchable) Capabilities() mail.Set { return mail.DerivedCapabilities(s) }
func (searchable) Quirks() []mail.Quirk     { return nil }

func (s searchable) Search(context.Context, mail.Query, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{Items: s.messages}, nil
}

func (s searchable) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{ID: id, Account: "work", Subject: "quarterly numbers", Date: time.Now()}, nil
}

// sender accepts anything it is given, so a send test is about what comes back rather than
// about what reached the provider.
type sender struct{}

func (sender) ID() mail.ProviderID      { return mail.ProviderGmail }
func (s sender) Capabilities() mail.Set { return mail.DerivedCapabilities(s) }
func (sender) Quirks() []mail.Quirk     { return nil }

func (sender) Send(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	return mail.ScopedID{Account: out.Account, Native: "sent_1"}, nil
}

func message(account mail.AccountID, alias, native string, at time.Time) mail.Message {
	id := mail.ScopedID{Account: account, Native: native}
	return mail.Message{ID: id, ThreadID: id, Account: alias, Subject: "hello", Date: at}
}

// Discovery is where a caller learns what it is holding, so it is where the two names for a
// mailbox have to appear together.
func TestAccountsListingNamesEveryAddress(t *testing.T) {
	tools := fanoutTools(byAccount{
		workMailbox.ID: searchable{}, archiveMailbox.ID: searchable{},
	})

	res, _, err := tools.handleAccounts(grantOver(mail.CapRead), nil, accountsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("discovery needs no capability: %v", payload(t, res))
	}

	body := payload(t, res)
	listed, ok := body["accounts"].([]any)
	if !ok || len(listed) != 2 {
		t.Fatalf("want both mailboxes listed, got %v", body["accounts"])
	}

	addresses := map[string]string{}
	for _, row := range listed {
		info := row.(map[string]any)
		addresses[info["alias"].(string)], _ = info["address"].(string)
	}
	if addresses["work"] != workMailbox.Address {
		t.Errorf("work should report %q, got %q", workMailbox.Address, addresses["work"])
	}
	if addresses["archive"] != archiveMailbox.Address {
		t.Errorf("archive should report %q, got %q", archiveMailbox.Address, addresses["archive"])
	}

	// default_scope is a selector, not a description, so it stays bare aliases: whatever it
	// offers is what a caller will put back in `account`.
	scope, ok := body["default_scope"].([]any)
	if !ok || len(scope) != 2 {
		t.Fatalf("want a default scope naming both mailboxes, got %v", body["default_scope"])
	}
	for _, s := range scope {
		if name := s.(string); name != "work" && name != "archive" {
			t.Errorf("default_scope must stay selectable aliases, got %q", name)
		}
	}
}

// A send cannot be taken back once the caller has read the result, so this is the result
// that most needs to say which mailbox it left from — the address is what the recipient will
// see it came from.
func TestSendNamesTheMailboxItWentFrom(t *testing.T) {
	res, _, err := toolsFor(sender{}).handleSend(grantWith(mail.CapSend), nil, sendArgs{
		composeArgs: composeArgs{
			To:      []addressArg{{Email: "colleague@example.com"}},
			Subject: "lunch",
			Body:    "Noon works.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the send was refused: %v", payload(t, res))
	}

	body := payload(t, res)
	if got := body["account"]; got != testAccount.Alias {
		t.Errorf("the alias must survive, got %v", got)
	}
	if got := body["account_address"]; got != testAccount.Address {
		t.Errorf("a send must name the address it went from, want %q, got %v",
			testAccount.Address, got)
	}
}

// The administrative tools act on exactly one mailbox and never fan out, so their result is
// the only place that mailbox is named at all.
func TestSettingsNameTheMailboxTheyRead(t *testing.T) {
	res, _, err := toolsFor(vacationOnly{}).handleSettings(grantWith(mail.CapSettings), nil,
		settingsArgs{Action: "vacation"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("reading the vacation responder was refused: %v", payload(t, res))
	}

	body := payload(t, res)
	if got := body["account"]; got != testAccount.Alias {
		t.Errorf("the alias must survive, got %v", got)
	}
	if got := body["account_address"]; got != testAccount.Address {
		t.Errorf("want the address %q beside the alias, got %v", testAccount.Address, got)
	}
}

// The aggregated block is keyed by alias, which is exactly why the address has to travel
// inside each entry: a caller reading "archive failed" has no other way to learn which
// mailbox archive is. Both halves of the block carry it — the one that worked and the one
// that did not.
func TestAggregatedOutcomesNameEveryMailbox(t *testing.T) {
	work := &stubLabels{}
	archive := &stubLabels{err: mail.ErrNeedsReauth}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: archive})

	res, _, err := tools.handleModify(grantOver(mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1", "acct_2:m2"}, AddLabels: []string{"Label_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("one failing mailbox must not lose the other: %v", payload(t, res))
	}

	accounts := accountsBlock(t, payload(t, res))
	if got := entry(t, accounts, "work")["address"]; got != workMailbox.Address {
		t.Errorf("the mailbox that was modified must name its address, want %q, got %v",
			workMailbox.Address, got)
	}
	if got := entry(t, accounts, "archive")["address"]; got != archiveMailbox.Address {
		t.Errorf("the mailbox that failed must name its address, want %q, got %v",
			archiveMailbox.Address, got)
	}
	// The failure still carries its code: naming the mailbox is added to that, not instead
	// of it.
	if got := entry(t, accounts, "archive")["error"]; got != "auth_expired" {
		t.Errorf("want auth_expired, got %v", got)
	}
}

// A merged search interleaves mailboxes, so both the status block and the rows themselves
// say which mailbox they came from. Reading a reply out of a work inbox and reading it out
// of a personal one are different situations that an alias alone does not separate.
func TestSearchResultsNameTheMailboxTheyCameFrom(t *testing.T) {
	now := time.Now()
	tools := fanoutTools(byAccount{
		workMailbox.ID: searchable{messages: []mail.Message{
			message(workMailbox.ID, "work", "m1", now),
		}},
		archiveMailbox.ID: searchable{messages: []mail.Message{
			message(archiveMailbox.ID, "archive", "m2", now.Add(-time.Hour)),
		}},
	})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil, searchArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the search was refused: %v", payload(t, res))
	}

	body := payload(t, res)
	accounts := accountsBlock(t, body)
	if got := entry(t, accounts, "work")["address"]; got != workMailbox.Address {
		t.Errorf("the status block must name each address, want %q, got %v",
			workMailbox.Address, got)
	}
	if got := entry(t, accounts, "archive")["address"]; got != archiveMailbox.Address {
		t.Errorf("the status block must name each address, want %q, got %v",
			archiveMailbox.Address, got)
	}

	rows, ok := body["results"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("want a row from each mailbox, got %v", body["results"])
	}
	found := map[string]string{}
	for _, r := range rows {
		row := r.(map[string]any)
		found[row["account"].(string)], _ = row["account_address"].(string)
	}
	if found["work"] != workMailbox.Address || found["archive"] != archiveMailbox.Address {
		t.Errorf("every merged row must name its mailbox's address, got %v", found)
	}
}

// The regression this change could plausibly cause. `account` accepts an alias or an
// address, and both must keep resolving — rendering a mailbox as "work - work@example.com"
// anywhere a caller reads a selector from would produce a string that is neither.
func TestAMailboxStillResolvesByAliasAndByAddress(t *testing.T) {
	for _, selector := range []string{"work", "work@example.com"} {
		t.Run(selector, func(t *testing.T) {
			tools := fanoutTools(byAccount{
				workMailbox.ID:    &stubLabels{labels: []mail.Label{{ID: "Label_1", Name: "receipts"}}},
				archiveMailbox.ID: &stubLabels{},
			})

			res, _, err := tools.handleLabels(grantOver(mail.CapRead), nil, labelsArgs{
				Action: "list", Account: selector,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("%q should still name the work mailbox: %v", selector, payload(t, res))
			}

			accounts := accountsBlock(t, payload(t, res))
			if len(accounts) != 1 {
				t.Fatalf("%q names one mailbox, got %v", selector, accounts)
			}
			if got := entry(t, accounts, "work")["address"]; got != workMailbox.Address {
				t.Errorf("want the work mailbox, got %v", accounts)
			}
		})
	}
}

// The combined form exists, and this is where it belongs: prose a person reads, never a
// value a caller is expected to hand back.
func TestARefusalNamesTheMailboxInFull(t *testing.T) {
	tools := fanoutTools(byAccount{workMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels), nil, labelsArgs{
		Action: "list", Account: "work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("listing labels without the read capability must be refused")
	}

	body := payload(t, res)
	if got := body["account"]; got != "work" {
		t.Errorf("the structured field stays the alias a caller selects by, got %v", got)
	}
	if got := body["account_address"]; got != workMailbox.Address {
		t.Errorf("want the address alongside, got %v", got)
	}
	if got, _ := body["message"].(string); !strings.Contains(got, "work - "+workMailbox.Address) {
		t.Errorf("the message a model relays should name the mailbox in full, got %q", got)
	}
}
