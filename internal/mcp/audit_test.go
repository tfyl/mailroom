package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

var errAuditDown = errors.New("database is locked")

type brokenAudit struct{}

func (brokenAudit) Record(context.Context, grant.Audit) error { return errAuditDown }

type recordingAudit struct{ entries []grant.Audit }

func (r *recordingAudit) Record(_ context.Context, e grant.Audit) error {
	r.entries = append(r.entries, e)
	return nil
}

func toolsAudited(auditor grant.Auditor, providers byAccount) *Tools {
	return NewTools(grant.NewGate(severalMailboxes{}, auditor, nil), providers, severalMailboxes{})
}

// stubReader is a mailbox that can be read.
type stubReader struct{ body string }

func (s stubReader) ID() mail.ProviderID    { return mail.ProviderGmail }
func (s stubReader) Capabilities() mail.Set { return mail.DerivedCapabilities(s) }
func (s stubReader) Quirks() []mail.Quirk   { return nil }

func (s stubReader) Search(context.Context, mail.Query, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

func (s stubReader) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{
		ID: id, Account: "work", Subject: "quarterly numbers", Date: time.Now(),
		Body: mail.Body{Text: s.body},
	}, nil
}

// An audit log that cannot be written is a reason to withhold a read, because withholding it
// still prevents the thing the row exists to attest. Every one of these call sites used to
// discard the error, so a broken log meant mail was read and nobody could ever say by whom.
func TestAReadIsWithheldWhenItCannotBeRecorded(t *testing.T) {
	const secret = "the numbers are in the attached deck"
	tools := toolsAudited(brokenAudit{}, byAccount{workMailbox.ID: stubReader{body: secret}})

	res, _, err := tools.handleGetMessage(grantOver(mail.CapRead), nil, idArgs{ID: "acct_1:m1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a read that could not be recorded must not be handed over")
	}

	body := payload(t, res)
	message, _ := body["message"].(string)
	if !strings.Contains(message, "audit log") {
		t.Errorf("the refusal should say why it was refused, got %q", message)
	}
	if strings.Contains(message, secret) {
		t.Error("the message content must not travel inside the refusal")
	}
}

// A change cannot be withheld: it has already happened. Failing the call would report a
// failure that did not occur and invite a retry that does the work twice, so the result says
// plainly that it went unrecorded.
func TestAChangeThatCannotBeRecordedIsStillReported(t *testing.T) {
	work := &stubLabels{}
	tools := toolsAudited(brokenAudit{}, byAccount{workMailbox.ID: work})

	res, _, err := tools.handleModify(grantOver(mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"}, AddLabels: []string{"Label_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the labels were applied, so the call did not fail: %v", payload(t, res))
	}
	if len(work.applied) != 1 {
		t.Fatalf("the modification should have reached the provider, got %d", len(work.applied))
	}

	entry := entry(t, accountsBlock(t, payload(t, res)), "work")
	note, _ := entry["not_recorded"].(string)
	if !strings.Contains(note, "audit log") {
		t.Errorf("an unrecorded change must say so in the result, got %v", entry)
	}
}

// Discovery reads the grant and unseals a credential per mailbox to ask what it supports.
// It wrote no audit row at all, against a security model that says every tool call writes
// one.
func TestDiscoveryWritesAnAuditRow(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{
		workMailbox.ID: stubReader{}, archiveMailbox.ID: stubReader{},
	})

	res, _, err := tools.handleAccounts(grantOver(mail.CapRead), nil, accountsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("discovery needs no capability: %v", payload(t, res))
	}

	seen := map[mail.AccountID]string{}
	for _, e := range auditor.entries {
		seen[e.AccountID] = e.Tool
	}
	for _, id := range []mail.AccountID{workMailbox.ID, archiveMailbox.ID} {
		if seen[id] != "mail.accounts" {
			t.Errorf("no audit row for %s, got %v", id, seen)
		}
	}
}

// The same rule, from the other side: discovery is a call, and a call that cannot be
// recorded does not answer.
func TestDiscoveryIsRefusedWhenItCannotBeRecorded(t *testing.T) {
	tools := toolsAudited(brokenAudit{}, byAccount{workMailbox.ID: stubReader{}})

	res, _, err := tools.handleAccounts(grantOver(mail.CapRead), nil, accountsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("discovery must be refused when it cannot be recorded")
	}
}
