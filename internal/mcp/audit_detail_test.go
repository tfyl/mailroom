package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// What the audit log is for is answering "what did this client do to my mail" after the mail
// has already changed, so these tests are about what a row holds rather than that one exists.
// The one below them all is the rule that bounds the rest: no message body, ever.

func onlyEntry(t *testing.T, a *recordingAudit, tool string) grant.Audit {
	t.Helper()
	var found []grant.Audit
	for _, e := range a.entries {
		if e.Tool == tool {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s row, got %d of %v", tool, len(found), a.entries)
	}
	return found[0]
}

// A send that recorded only "mail.send ok" left the operator unable to answer the one question
// the log exists for. The threat in docs/security.md is a message going somewhere it should
// not, and the address is the whole of what identifies that.
func TestASendRecordsWhereItWentAndWhatItWasCalled(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{workMailbox.ID: sender{}})

	res, _, err := tools.handleSend(grantOver(mail.CapSend), nil, sendArgs{composeArgs: composeArgs{
		Account: "work",
		To:      []addressArg{{Email: "priya@example.com", Name: "Priya"}},
		Cc:      []addressArg{{Email: "sam@partner.example"}},
		Bcc:     []addressArg{{Email: "records@example.com"}},
		Subject: "Re: quarterly numbers",
		Body:    "The deck is attached. Numbers are on slide four.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the send was refused: %v", payload(t, res))
	}

	e := onlyEntry(t, auditor, "mail.send")
	if e.Capability != mail.CapSend {
		t.Errorf("a send spends `send`; the row says %q", e.Capability)
	}
	if got := e.Detail.To; len(got) != 1 || got[0] != "priya@example.com" {
		t.Errorf("the recipient must be recorded, got %v", got)
	}
	if got := e.Detail.Cc; len(got) != 1 || got[0] != "sam@partner.example" {
		t.Errorf("cc must be recorded, got %v", got)
	}
	if got := e.Detail.Bcc; len(got) != 1 || got[0] != "records@example.com" {
		t.Errorf("bcc must be recorded — it is a recipient nobody else can see, got %v", got)
	}
	if e.Detail.Subject != "Re: quarterly numbers" {
		t.Errorf("the subject of outgoing mail is recorded, got %q", e.Detail.Subject)
	}
	if e.Affected == nil || *e.Affected != 3 {
		t.Errorf("how much, for a send, is how many people it reached; got %v", e.Affected)
	}
	if got := e.Detail.IDs; len(got) != 1 || !strings.HasPrefix(got[0], string(workMailbox.ID)+":") {
		t.Errorf("the sent message should be identifiable afterwards, got %v", got)
	}
}

// The same fact from the other side. A read is the high-volume path, and a log that copied a
// subject off every message read would become the mailbox rather than a record of it.
func TestAReadRecordsTheIDAndNothingOffTheMessage(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{workMailbox.ID: stubReader{body: "nothing to see"}})

	res, _, err := tools.handleGetMessage(grantOver(mail.CapRead), nil, idArgs{ID: "acct_1:m1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the read was refused: %v", payload(t, res))
	}

	e := onlyEntry(t, auditor, "mail.get_message")
	if e.Capability != mail.CapRead {
		t.Errorf("a read spends `read`; the row says %q", e.Capability)
	}
	if got := e.Detail.IDs; len(got) != 1 || got[0] != "acct_1:m1" {
		t.Errorf("the message read must be identifiable afterwards, got %v", got)
	}
	// stubReader answers with the subject "quarterly numbers". None of it belongs here.
	if e.Detail.Subject != "" {
		t.Errorf("the subject of a message that was read must not be recorded, got %q", e.Detail.Subject)
	}
	if len(e.Detail.To) != 0 || len(e.Detail.Cc) != 0 || len(e.Detail.Bcc) != 0 {
		t.Errorf("a read has no recipients to record, got %+v", e.Detail)
	}
}

// A modify row said that something was labelled and never what, which is the one fact that
// may no longer be recoverable from the mailbox by the time anybody looks.
func TestModifyRecordsWhatChangedAndOnWhat(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{workMailbox.ID: &stubLabels{}})

	read := true
	res, _, err := tools.handleModify(grantOver(mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1", "acct_1:m2"}, AddLabels: []string{"Label_receipts"},
		Archive: true, Read: &read,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the modify was refused: %v", payload(t, res))
	}

	e := onlyEntry(t, auditor, "mail.modify")
	if e.Affected == nil || *e.Affected != 2 {
		t.Errorf("want 2 messages counted, got %v", e.Affected)
	}
	if len(e.Detail.IDs) != 2 {
		t.Errorf("want both ids recorded, got %v", e.Detail.IDs)
	}
	for _, want := range []string{"+Label_receipts", "-INBOX", "+read"} {
		if !strings.Contains(e.Detail.Action, want) {
			t.Errorf("the change should name %s, got %q", want, e.Detail.Action)
		}
	}
}

// A search is the one read that must not name what it found. Its result set is "everything
// matching a query", so recording the ids would put a list of the mailbox in the log on every
// call. The count is what answers "how much".
func TestSearchRecordsHowManyAndNotWhich(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{
		workMailbox.ID: searchable{messages: []mail.Message{
			message(workMailbox.ID, "work", "m1", time.Now()),
			message(workMailbox.ID, "work", "m2", time.Now().Add(-time.Minute)),
		}},
	})

	res, _, err := tools.handleSearch(grantOver(mail.CapRead), nil, searchArgs{Account: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the search was refused: %v", payload(t, res))
	}

	e := onlyEntry(t, auditor, "mail.search")
	if e.Affected == nil || *e.Affected != 2 {
		t.Errorf("want 2 results counted, got %v", e.Affected)
	}
	if len(e.Detail.IDs) != 0 {
		t.Errorf("a search must not record what it matched, got %v", e.Detail.IDs)
	}
	if e.Detail.Subject != "" {
		t.Errorf("a search must not record subjects, got %q", e.Detail.Subject)
	}
}

// Discovery spends no capability. That is a fact about the call, and recording it as empty is
// how the page can say "none required" rather than leaving a gap a reader has to interpret.
func TestDiscoveryRecordsNoCapability(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{workMailbox.ID: stubReader{}})

	if _, _, err := tools.handleAccounts(grantOver(mail.CapRead), nil, accountsArgs{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range auditor.entries {
		if e.Tool == "mail.accounts" && e.Capability != "" {
			t.Errorf("discovery spends nothing; the row claims %q", e.Capability)
		}
	}
}

// The gate used to turn a call away and write nothing, so the page an operator opens to find
// out what a client was refused showed provider failures and nothing else. The two failures
// have to arrive distinguishable, because one is fixed on the grants page and the other is
// somebody else's outage.
func TestARefusalSaysWhoRefusedIt(t *testing.T) {
	t.Run("the gate", func(t *testing.T) {
		auditor := &recordingAudit{}
		tools := toolsAudited(auditor, byAccount{workMailbox.ID: sender{}})

		// A grant with `read` and not `send`, asked to send.
		res, _, err := tools.handleSend(grantOver(mail.CapRead), nil, sendArgs{composeArgs: composeArgs{
			Account: "work", To: []addressArg{{Email: "priya@example.com"}}, Subject: "hello",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("a grant without `send` must not be able to send")
		}

		e := onlyEntry(t, auditor, "mail.send")
		if e.Outcome != "scope_denied" {
			t.Errorf("a refusal by the gate is scope_denied, got %q", e.Outcome)
		}
		if !strings.Contains(e.Reason, "requires") {
			t.Errorf("the reason should say what was missing, got %q", e.Reason)
		}
	})

	t.Run("the provider", func(t *testing.T) {
		auditor := &recordingAudit{}
		broken := &stubLabels{err: &mail.ProviderError{
			Provider: mail.ProviderGmail, Account: "work", Op: "store", Err: errUpstream,
		}}
		tools := toolsAudited(auditor, byAccount{workMailbox.ID: broken})

		res, _, err := tools.handleModify(grantOver(mail.CapLabels), nil, modifyArgs{
			IDs: []string{"acct_1:m1"}, AddLabels: []string{"Label_1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("every mailbox failed, so the call failed")
		}

		e := onlyEntry(t, auditor, "mail.modify")
		if e.Outcome != "provider_error" {
			t.Errorf("an upstream failure is a provider_error, got %q", e.Outcome)
		}
		if !strings.Contains(e.Reason, "connection reset") {
			t.Errorf("the reason should carry what the provider said, got %q", e.Reason)
		}
	})

	// A client sending a call this server refuses on its arguments alone is a client with a
	// bug, and reading that as a mailbox problem sends somebody to debug the wrong system.
	t.Run("this server, on the arguments", func(t *testing.T) {
		auditor := &recordingAudit{}
		tools := toolsAudited(auditor, byAccount{workMailbox.ID: sender{}})

		res, _, err := tools.handleSend(grantOver(mail.CapSend), nil, sendArgs{
			composeArgs: composeArgs{Account: "work", Subject: "nobody"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("a send with no recipient must be refused")
		}

		e := onlyEntry(t, auditor, "mail.send")
		if e.Outcome != grant.OutcomeInvalid {
			t.Errorf("a malformed call is %q, got %q", grant.OutcomeInvalid, e.Outcome)
		}
		if !strings.Contains(e.Reason, "recipient") {
			t.Errorf("the reason should say what was wrong with the call, got %q", e.Reason)
		}
	})
}

// A batch a grant cannot reach is one refused call, not fifty. A row per rejected id would let
// a single confused client fill the page an operator reads by scanning it.
func TestARefusedBatchIsOneRow(t *testing.T) {
	auditor := &recordingAudit{}
	tools := toolsAudited(auditor, byAccount{workMailbox.ID: &stubLabels{}})

	ids := make([]string, 20)
	for i := range ids {
		ids[i] = "acct_3:m" + string(rune('a'+i)) // a real mailbox this grant does not name
	}
	if _, _, err := tools.handleModify(grantOver(mail.CapLabels), nil, modifyArgs{
		IDs: ids, AddLabels: []string{"Label_1"},
	}); err != nil {
		t.Fatal(err)
	}

	e := onlyEntry(t, auditor, "mail.modify")
	if e.Affected == nil || *e.Affected != 20 {
		t.Errorf("the row should count every id it refused, got %v", e.Affected)
	}
	if e.Outcome == "ok" {
		t.Error("a batch that reached no mailbox is not ok")
	}
}

// The rule the whole design rests on, asserted from the outside: whatever a tool is given, no
// part of a message body reaches the auditor. grant.Detail has no field that would hold one,
// which is what makes this hold rather than the care of whoever adds the next tool.
func TestNoMessageBodyReachesTheAuditLog(t *testing.T) {
	const body = "PRIVATE-BODY-MARKER the wire transfer details are 12-34-56 87654321"

	cases := []struct {
		name string
		call func(*Tools) error
	}{
		{"send", func(tools *Tools) error {
			_, _, err := tools.handleSend(grantOver(mail.CapSend), nil, sendArgs{composeArgs: composeArgs{
				Account: "work", To: []addressArg{{Email: "priya@example.com"}},
				Subject: "payment", Body: body, HTML: "<p>" + body + "</p>",
			}})
			return err
		}},
		{"draft", func(tools *Tools) error {
			_, _, err := tools.handleDraft(grantOver(mail.CapDraft), nil, draftArgs{composeArgs: composeArgs{
				Account: "work", To: []addressArg{{Email: "priya@example.com"}},
				Subject: "payment", Body: body, HTML: "<p>" + body + "</p>",
			}})
			return err
		}},
		{"get_message", func(tools *Tools) error {
			_, _, err := tools.handleGetMessage(grantOver(mail.CapRead), nil, idArgs{ID: "acct_1:m1"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auditor := &recordingAudit{}
			tools := toolsAudited(auditor, byAccount{workMailbox.ID: bodyProvider{body: body}})
			if err := tc.call(tools); err != nil {
				t.Fatal(err)
			}
			if len(auditor.entries) == 0 {
				t.Fatal("the call recorded nothing, so this proves nothing")
			}
			for _, e := range auditor.entries {
				recorded, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(recorded), "PRIVATE-BODY-MARKER") {
					t.Fatalf("a message body reached the audit log: %s", recorded)
				}
			}
		})
	}
}

// bodyProvider carries the marker body in every direction a body can travel: it answers reads
// with one and accepts writes carrying one.
type bodyProvider struct{ body string }

func (bodyProvider) ID() mail.ProviderID      { return mail.ProviderGmail }
func (b bodyProvider) Capabilities() mail.Set { return mail.DerivedCapabilities(b) }
func (bodyProvider) Quirks() []mail.Quirk     { return nil }

func (b bodyProvider) Search(context.Context, mail.Query, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

func (b bodyProvider) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{
		ID: id, Account: "work", Subject: b.body,
		From: mail.Address{Email: "stranger@example.net", Name: b.body},
		Body: mail.Body{Text: b.body, HTML: "<p>" + b.body + "</p>"},
	}, nil
}

func (b bodyProvider) Send(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	return mail.ScopedID{Account: out.Account, Native: "sent_1"}, nil
}

func (b bodyProvider) CreateDraft(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	return mail.ScopedID{Account: out.Account, Native: "draft_1"}, nil
}

func (b bodyProvider) UpdateDraft(context.Context, mail.ScopedID, mail.Outgoing) error { return nil }
func (b bodyProvider) DeleteDraft(context.Context, mail.ScopedID) error                { return nil }

func (b bodyProvider) SendDraft(_ context.Context, id mail.ScopedID) (mail.ScopedID, error) {
	return id, nil
}

func (b bodyProvider) ListDrafts(context.Context, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}
