package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

var testAccount = mail.Account{
	ID: "acct_1", OwnerID: "u1", Alias: "work", Address: "operator@example.com",
	Provider: mail.ProviderGmail, Status: mail.StatusLinked,
}

// oneMailbox stands in for the store: a single mailbox, found by any of the three lookups.
type oneMailbox struct{}

func (oneMailbox) Account(_ context.Context, _ user.ID, id mail.AccountID) (mail.Account, error) {
	if id != testAccount.ID {
		return mail.Account{}, mail.ErrNotFound
	}
	return testAccount, nil
}

func (oneMailbox) AccountByAlias(_ context.Context, _ user.ID, alias string) (mail.Account, error) {
	if alias != testAccount.Alias {
		return mail.Account{}, mail.ErrNotFound
	}
	return testAccount, nil
}

func (oneMailbox) AccountByAddress(_ context.Context, _ user.ID, address string) (mail.Account, error) {
	if address != testAccount.Address {
		return mail.Account{}, mail.ErrNotFound
	}
	return testAccount, nil
}

type silentAudit struct{}

func (silentAudit) Record(context.Context, grant.Audit) error { return nil }

type oneProvider struct{ p mail.Provider }

func (o oneProvider) For(context.Context, mail.Account) (mail.Provider, error) { return o.p, nil }

func toolsFor(p mail.Provider) *Tools {
	return NewTools(grant.NewGate(oneMailbox{}, silentAudit{}, nil), oneProvider{p}, oneMailbox{})
}

func grantWith(caps ...mail.Capability) context.Context {
	return context.WithValue(context.Background(), grantKey{}, &grant.Grant{
		ID: "g1", OwnerID: "u1", Accounts: []mail.AccountID{testAccount.ID}, Caps: mail.NewSet(caps...),
	})
}

// payload reads the JSON a tool answered with. Content carries the untrusted-data notice
// first on a success, so the body is always the last block.
func payload(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("the tool returned no content")
	}
	text, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want text content, got %T", res.Content[len(res.Content)-1])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("the tool answered with something that is not JSON: %v\n%s", err, text.Text)
	}
	return out
}

// recordingDrafts keeps whatever it was last asked to write, so a test can assert on the
// message that would have reached the provider. It also records deletions, because the
// interesting half of a refused delete is that the provider was never asked.
type recordingDrafts struct {
	updated mail.Outgoing
	deleted mail.ScopedID
	sent    mail.ScopedID
}

func (r *recordingDrafts) ID() mail.ProviderID    { return mail.ProviderGmail }
func (r *recordingDrafts) Capabilities() mail.Set { return mail.DerivedCapabilities(r) }
func (r *recordingDrafts) Quirks() []mail.Quirk   { return nil }

func (r *recordingDrafts) DeleteDraft(_ context.Context, id mail.ScopedID) error {
	r.deleted = id
	return nil
}

func (r *recordingDrafts) CreateDraft(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	r.updated = out
	return mail.ScopedID{Account: testAccount.ID, Native: "draft_1"}, nil
}

func (r *recordingDrafts) UpdateDraft(_ context.Context, _ mail.ScopedID, out mail.Outgoing) error {
	r.updated = out
	return nil
}

func (r *recordingDrafts) SendDraft(_ context.Context, id mail.ScopedID) (mail.ScopedID, error) {
	r.sent = id
	return id, nil
}

func (r *recordingDrafts) ListDrafts(context.Context, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

// Revising a reply is the ordinary flow — draft it, read it back, change a line, send it —
// and an update rebuilds the whole message from what it is given. The update path passed an
// empty in_reply_to while holding the caller's, so the revised reply lost its threading
// headers and arrived as a new conversation rather than in the thread it answers.
func TestUpdatingAReplyKeepsItInItsThread(t *testing.T) {
	drafts := &recordingDrafts{}
	res, _, err := toolsFor(drafts).handleDraft(grantWith(mail.CapDraft), nil, draftArgs{
		Action:  "update",
		DraftID: "acct_1:draft_1",
		composeArgs: composeArgs{
			InReplyTo: "acct_1:msg_5",
			To:        []addressArg{{Email: "colleague@example.com"}},
			Subject:   "Re: lunch",
			Body:      "Noon works.",
		},
	})
	if err != nil {
		t.Fatalf("updating the draft: %v", err)
	}
	if res.IsError {
		t.Fatalf("the update was refused: %v", payload(t, res))
	}
	if got := drafts.updated.InReplyTo.String(); got != "acct_1:msg_5" {
		t.Errorf("the reply lost its thread: in_reply_to reached the provider as %q", got)
	}
}

// The draft's own id says which mailbox this is, so a reply target in another one is a
// mistake rather than a request — and answering it would thread a draft onto a conversation
// its mailbox has never seen.
func TestUpdatingADraftRefusesAReplyTargetInAnotherMailbox(t *testing.T) {
	drafts := &recordingDrafts{}
	res, _, err := toolsFor(drafts).handleDraft(grantWith(mail.CapDraft), nil, draftArgs{
		Action:      "update",
		DraftID:     "acct_1:draft_1",
		composeArgs: composeArgs{InReplyTo: "acct_2:msg_5", Body: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a reply target in a different mailbox must be refused")
	}
	if !drafts.updated.InReplyTo.Zero() {
		t.Error("the provider must not be asked to write anything after a refusal")
	}
}

// noSettings has no settings at all, the way IMAP does not.
type noSettings struct{}

func (noSettings) ID() mail.ProviderID    { return mail.ProviderIMAP }
func (noSettings) Capabilities() mail.Set { return mail.NewSet() }
func (noSettings) Quirks() []mail.Quirk   { return nil }

// vacationOnly implements the settings every provider with settings has, and none of the
// rarer ones — the shape a consumer Gmail account presents for delegation.
type vacationOnly struct{ noSettings }

func (vacationOnly) ListSendAs(context.Context) ([]mail.SendAs, error) { return nil, nil }
func (vacationOnly) GetVacation(context.Context) (mail.Vacation, error) {
	return mail.Vacation{}, nil
}
func (vacationOnly) SetVacation(context.Context, mail.Vacation) error { return nil }

// A refusal a client cannot recognise is a refusal it retries. mail_settings formatted its
// unsupported error into a string, which flattened the code to a generic `error` — the one
// the taxonomy tells clients may be worth trying again — so a client matching on the
// documented code retried a permanent failure forever.
func TestUnsupportedSettingsKeepTheirCode(t *testing.T) {
	cases := []struct {
		name     string
		provider mail.Provider
		action   string
		names    string
	}{
		{"no settings at all", noSettings{}, "vacation", "settings"},
		{"one section missing", vacationOnly{}, "forwarding", "forwarding settings"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, err := toolsFor(tc.provider).handleSettings(grantWith(mail.CapSettings), nil,
				settingsArgs{Action: tc.action})
			if err != nil {
				t.Fatal(err)
			}
			body := payload(t, res)
			if body["error"] != "unsupported_by_provider" {
				t.Errorf("want the unsupported_by_provider code, got %q", body["error"])
			}
			message, _ := body["message"].(string)
			if !strings.Contains(message, tc.names) {
				t.Errorf("the message should name %q; got %q", tc.names, message)
			}
		})
	}
}

// --- composing and destroying are different decisions ---------------------------------
//
// `draft` used to mean create, edit and delete, so an agent trusted to write a reply was
// also trusted to remove a draft a person had written. These four tests are the split: the
// compose half, the destroy half, the grant that predates the split, and the one path that
// removes a draft and must not need the destroy half.

// The refusal that is the whole point. Composing does not imply destroying, and the proof
// that it is a refusal rather than a silent no-op is that the provider was never asked.
func TestAComposeGrantCannotDeleteADraft(t *testing.T) {
	drafts := &recordingDrafts{}
	res, _, err := toolsFor(drafts).handleDraft(grantWith(mail.CapDraft), nil, draftArgs{
		Action:  "delete",
		DraftID: "acct_1:draft_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a grant holding draft but not discard must not be able to delete a draft")
	}
	if got := payload(t, res)["message"]; !strings.Contains(fmt.Sprint(got), string(mail.CapDiscard)) {
		t.Errorf("the refusal should name the capability that is missing, got %v", got)
	}
	if !drafts.deleted.Zero() {
		t.Errorf("the provider must not be asked to delete anything after a refusal, got %v", drafts.deleted)
	}
}

// The other side of the same grant: taking deletion away must not take composing with it,
// or the split has simply removed the capability people actually use.
func TestAComposeGrantStillCreatesAndUpdatesDrafts(t *testing.T) {
	drafts := &recordingDrafts{}
	tools := toolsFor(drafts)

	created, _, err := tools.handleDraft(grantWith(mail.CapDraft), nil, draftArgs{
		composeArgs: composeArgs{
			To: []addressArg{{Email: "colleague@example.com"}}, Subject: "lunch", Body: "Noon?",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.IsError {
		t.Fatalf("creating a draft was refused: %v", payload(t, created))
	}
	if got := payload(t, created)["draft_id"]; got != "acct_1:draft_1" {
		t.Errorf("want the new draft's id back, got %v", got)
	}

	updated, _, err := tools.handleDraft(grantWith(mail.CapDraft), nil, draftArgs{
		Action:      "update",
		DraftID:     "acct_1:draft_1",
		composeArgs: composeArgs{Subject: "lunch", Body: "One o'clock?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.IsError {
		t.Fatalf("updating a draft was refused: %v", payload(t, updated))
	}
	if drafts.updated.Body.Text != "One o'clock?" {
		t.Errorf("the revision did not reach the provider, got %q", drafts.updated.Body.Text)
	}
}

// The capability has to actually work on its own. A grant may hold discard without draft —
// the consent screen offers every combination — and mail_draft is registered for either.
func TestADiscardGrantDeletesADraft(t *testing.T) {
	drafts := &recordingDrafts{}
	res, _, err := toolsFor(drafts).handleDraft(grantWith(mail.CapDiscard), nil, draftArgs{
		Action:  "delete",
		DraftID: "acct_1:draft_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a grant holding discard must be able to delete a draft: %v", payload(t, res))
	}
	if drafts.deleted.String() != "acct_1:draft_1" {
		t.Errorf("the provider was asked to delete %v", drafts.deleted)
	}
}

// What happens to the grants that already exist, stated as a test rather than as a promise.
//
// A grant is stored as a comma-separated capability list, so one approved before this change
// reads back as exactly the words that were written to the row. None of them is `discard`,
// and nothing re-grants it on the way through: an agent that could delete a draft yesterday
// is refused today, until the operator widens the grant from the grants page. That is the
// behaviour change this split makes deliberately, and it is fail-closed on purpose.
func TestAGrantRecordedBeforeTheSplitCannotDeleteADraft(t *testing.T) {
	stored, err := mail.ParseSet("read,draft,send")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Has(mail.CapDiscard) {
		t.Fatal("a stored grant must not gain discard by being read back")
	}

	drafts := &recordingDrafts{}
	ctx := context.WithValue(context.Background(), grantKey{}, &grant.Grant{
		ID: "g_old", OwnerID: "u1", Accounts: []mail.AccountID{testAccount.ID}, Caps: stored,
	})
	res, _, err := toolsFor(drafts).handleDraft(ctx, nil, draftArgs{
		Action: "delete", DraftID: "acct_1:draft_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a grant approved before discard existed must not delete drafts")
	}
	if !drafts.deleted.Zero() {
		t.Error("the provider must not be asked to delete anything after a refusal")
	}
}

// Sending a draft removes it — that is what sending a draft is — and that removal is part of
// `send`, not of `discard`. Requiring discard here would have broken send for every grant
// that existed before this change, which is every grant on a live instance.
func TestSendingADraftDoesNotNeedDiscard(t *testing.T) {
	drafts := &recordingDrafts{}
	res, _, err := toolsFor(drafts).handleSend(grantWith(mail.CapDraft, mail.CapSend), nil, sendArgs{
		DraftID: "acct_1:draft_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("sending a draft must not need discard: %v", payload(t, res))
	}
	if drafts.sent.String() != "acct_1:draft_1" {
		t.Errorf("the provider was asked to send %v", drafts.sent)
	}
}
