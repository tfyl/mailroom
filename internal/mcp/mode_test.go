package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// The two halves of a mode, tested as the two different things they are.
//
// Steering is what a tool's description says, and the only claim worth making about it is
// that the right words reach the client — a model that reads "wait for an answer" and sends
// anyway is outside anything this package can assert. So those tests read descriptions.
//
// Enforcement is what the server refuses, and the claim is much stronger: under `hold`, a
// send does not reach the provider. Those tests watch the provider.

// --- fixtures --------------------------------------------------------------------------

// memoryQueue is the held store, in memory. The real one is exercised against SQLite in
// internal/store and end to end in internal/web; what these tests need from it is only that
// an action goes in and nothing comes out on its own.
type memoryQueue struct {
	mu      sync.Mutex
	actions []held.Action
}

func (q *memoryQueue) HoldAction(_ context.Context, _ user.ID, a held.Action) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.actions = append(q.actions, a)
	return nil
}

func (q *memoryQueue) PendingActions(_ context.Context, _ user.ID, _ time.Time) ([]held.Action, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]held.Action(nil), q.actions...), nil
}

func (q *memoryQueue) RecentActions(context.Context, user.ID, int) ([]held.Action, error) {
	return nil, nil
}

func (q *memoryQueue) CountPending(_ context.Context, _ user.ID, _ grant.ID, _ time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.actions), nil
}

func (q *memoryQueue) ClaimAction(context.Context, user.ID, string, string, time.Time) (held.Action, error) {
	return held.Action{}, held.ErrNotPending
}

func (q *memoryQueue) ExpireActions(context.Context, time.Time) (int, error) { return 0, nil }

func (q *memoryQueue) MarkFailed(context.Context, user.ID, string, string) error { return nil }

func (q *memoryQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.actions)
}

// sendingProvider records what actually reached the mail server. Under `hold` this must stay
// empty, which is the whole assertion.
type sendingProvider struct {
	stubLabels
	mu       sync.Mutex
	sent     []mail.Outgoing
	trashed  [][]mail.ScopedID
	deleted  [][]mail.ScopedID
	restored [][]mail.ScopedID
	filters  []mail.Filter
	dropped  []string
	vacation []mail.Vacation
}

func (p *sendingProvider) ID() mail.ProviderID    { return mail.ProviderGmail }
func (p *sendingProvider) Capabilities() mail.Set { return mail.DerivedCapabilities(p) }
func (p *sendingProvider) Quirks() []mail.Quirk   { return nil }

func (p *sendingProvider) Send(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, out)
	return mail.ScopedID{Account: testAccount.ID, Native: "sent_1"}, nil
}

func (p *sendingProvider) CreateDraft(context.Context, mail.Outgoing) (mail.ScopedID, error) {
	return mail.ScopedID{Account: testAccount.ID, Native: "draft_1"}, nil
}
func (p *sendingProvider) UpdateDraft(context.Context, mail.ScopedID, mail.Outgoing) error {
	return nil
}
func (p *sendingProvider) DeleteDraft(context.Context, mail.ScopedID) error { return nil }
func (p *sendingProvider) ListDrafts(context.Context, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

func (p *sendingProvider) SendDraft(_ context.Context, id mail.ScopedID) (mail.ScopedID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, mail.Outgoing{Subject: "the draft " + id.String()})
	return id, nil
}

func (p *sendingProvider) Trash(_ context.Context, ids []mail.ScopedID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trashed = append(p.trashed, ids)
	return nil
}

func (p *sendingProvider) Untrash(_ context.Context, ids []mail.ScopedID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restored = append(p.restored, ids)
	return nil
}

func (p *sendingProvider) Delete(_ context.Context, ids []mail.ScopedID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, ids)
	return nil
}

func (p *sendingProvider) ListFilters(context.Context) ([]mail.Filter, error) { return nil, nil }

func (p *sendingProvider) CreateFilter(_ context.Context, f mail.Filter) (mail.Filter, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filters = append(p.filters, f)
	f.ID = "filter_1"
	return f, nil
}

func (p *sendingProvider) DeleteFilter(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropped = append(p.dropped, id)
	return nil
}

func (p *sendingProvider) ListSendAs(context.Context) ([]mail.SendAs, error) { return nil, nil }
func (p *sendingProvider) GetVacation(context.Context) (mail.Vacation, error) {
	return mail.Vacation{}, nil
}

func (p *sendingProvider) SetVacation(_ context.Context, v mail.Vacation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vacation = append(p.vacation, v)
	return nil
}

func (p *sendingProvider) touched() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent) + len(p.trashed) + len(p.deleted) + len(p.filters) +
		len(p.dropped) + len(p.vacation)
}

// modeRig is a grant in one mode, its tools, the queue behind them and the provider in front.
type modeRig struct {
	tools    *Tools
	grant    *grant.Grant
	queue    *memoryQueue
	provider *sendingProvider
}

func newModeRig(t *testing.T, mode grant.Mode, caps ...mail.Capability) modeRig {
	t.Helper()
	if len(caps) == 0 {
		caps = mail.AllCapabilities
	}
	provider := &sendingProvider{}
	queue := &memoryQueue{}
	tools := NewTools(grant.NewGate(oneMailbox{}, silentAudit{}, nil), oneProvider{provider}, oneMailbox{}).
		WithHoldQueue(held.New(queue, oneProvider{provider}, oneMailbox{}, silentAudit{}, time.Hour),
			"https://mail.example.com")
	return modeRig{
		tools: tools,
		grant: &grant.Grant{
			ID: "g1", OwnerID: "u1", Accounts: []mail.AccountID{testAccount.ID},
			Caps: mail.NewSet(caps...), Mode: mode,
		},
		queue:    queue,
		provider: provider,
	}
}

func (r modeRig) ctx() context.Context {
	return context.WithValue(context.Background(), grantKey{}, r.grant)
}

// descriptions is what a client sees in tools/list, over the real protocol.
func descriptions(t *testing.T, tools *Tools, g *grant.Grant) map[string]string {
	t.Helper()

	srv := NewServer(fixedGrant{g}, tools, "https://mail.example.com",
		slog.New(slog.NewTextHandler(discard{}, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: "token", base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// --- steering --------------------------------------------------------------------------

// The advisory half. Every mode has to produce its own wording for the tools it governs, and
// a client that switched connections must see a different instruction rather than the same
// paragraph three times.
func TestEachModeProducesItsOwnSteering(t *testing.T) {
	byMode := map[grant.Mode]map[string]string{}
	for _, m := range grant.AllModes {
		rig := newModeRig(t, m)
		byMode[m] = descriptions(t, rig.tools, rig.grant)
	}

	for _, tool := range []string{
		"mail_accounts", "mail_send", "mail_trash", "mail_filters", "mail_settings",
	} {
		seen := map[string]grant.Mode{}
		for _, m := range grant.AllModes {
			text := byMode[m][tool]
			if text == "" {
				t.Fatalf("%s is not registered, or has no description, under %s", tool, m)
			}
			if other, clash := seen[text]; clash {
				t.Errorf("%s reads identically under %s and %s, so the mode is telling the "+
					"client nothing:\n%s", tool, other, m, text)
			}
			seen[text] = m
		}
	}
}

// The description of a held tool has one job beyond steering, and it is the one that would be
// a defect to get wrong: a client whose send is going to be queued must be told so, or it will
// report the mail as sent on the strength of a successful call.
func TestHoldSteeringSaysTheCallDoesNotSend(t *testing.T) {
	rig := newModeRig(t, grant.ModeHold)
	got := descriptions(t, rig.tools, rig.grant)

	send := got["mail_send"]
	for _, want := range []string{"does not deliver", "queued", "approve", "waiting for approval"} {
		if !strings.Contains(send, want) {
			t.Errorf("mail_send under hold never says %q:\n%s", want, send)
		}
	}
	// The call every client is told to make first has to carry the posture, for the client
	// that read its tool list once and cached it.
	accounts := got["mail_accounts"]
	if !strings.Contains(accounts, "not carried out") || !strings.Contains(accounts, "hold") {
		t.Errorf("mail_accounts under hold does not describe the mode:\n%s", accounts)
	}

	// And the opposite: nothing under the loose mode may imply a queue that is not there.
	loose := descriptions(t, newModeRig(t, grant.ModeUnattended).tools,
		newModeRig(t, grant.ModeUnattended).grant)
	if strings.Contains(loose["mail_send"], "queued") {
		t.Errorf("mail_send under unattended promises a queue that does not exist:\n%s",
			loose["mail_send"])
	}
}

// The steering a grant with nothing recorded gets is the default's, word for word. Anything
// else would mean an upgraded install quietly telling its clients something new.
func TestAGrantWithNoModeIsSteeredAsBalanced(t *testing.T) {
	none := newModeRig(t, "")
	balanced := newModeRig(t, grant.ModeConfirm)

	got := descriptions(t, none.tools, none.grant)
	want := descriptions(t, balanced.tools, balanced.grant)
	for tool, text := range want {
		if got[tool] != text {
			t.Errorf("%s reads differently for a grant with no mode:\n got: %s\nwant: %s",
				tool, got[tool], text)
		}
	}
}

// --- enforcement -----------------------------------------------------------------------

func sendArgsTo(address string) sendArgs {
	return sendArgs{composeArgs: composeArgs{
		Account: "work",
		To:      []addressArg{{Email: address}},
		Subject: "the quarterly numbers",
		Body:    "Attached.",
	}}
}

// The claim the whole mode rests on: under `hold`, a send does not reach the mail server.
func TestInHoldModeASendIsQueuedAndNeverReachesTheProvider(t *testing.T) {
	rig := newModeRig(t, grant.ModeHold)

	res, _, err := rig.tools.handleSend(rig.ctx(), nil, sendArgsTo("finance@example.com"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.IsError {
		t.Fatalf("a held send is not an error, it is a queued one: %v", payload(t, res))
	}

	if len(rig.provider.sent) != 0 {
		t.Fatalf("the message reached the provider: %+v", rig.provider.sent)
	}
	if rig.queue.len() != 1 {
		t.Fatalf("want one action waiting, got %d", rig.queue.len())
	}

	// The result has to say so. A client that reads `sent` here tells its user the mail has
	// gone, which is the failure this mode would otherwise introduce.
	body := payload(t, res)
	if body["held"] != true {
		t.Errorf("the result does not say the call was held: %v", body)
	}
	if _, ok := body["sent"]; ok {
		t.Errorf("the result claims something was sent: %v", body)
	}
	if id, _ := body["action_id"].(string); id == "" {
		t.Error("a held action has to come back with the id it was queued under")
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "Nothing was done") {
		t.Errorf("the note should say plainly that nothing happened: %q", note)
	}

	// And what was queued is the message, not a description of it.
	var queued held.SendPayload
	decodeAction(t, rig.queue.actions[0], &queued)
	if len(queued.Outgoing.To) != 1 || queued.Outgoing.To[0].Email != "finance@example.com" {
		t.Errorf("the queued message lost its recipients: %+v", queued.Outgoing)
	}
	if queued.Outgoing.Subject != "the quarterly numbers" {
		t.Errorf("the queued message lost its subject: %+v", queued.Outgoing)
	}
}

// The other two modes send. That is the point of them being different modes, and a test that
// only proved the strict one holds would not distinguish this feature from a broken send.
func TestTheOtherModesSendStraightAway(t *testing.T) {
	for _, m := range []grant.Mode{grant.ModeUnattended, grant.ModeConfirm, ""} {
		rig := newModeRig(t, m)
		res, _, err := rig.tools.handleSend(rig.ctx(), nil, sendArgsTo("finance@example.com"))
		if err != nil {
			t.Fatalf("%q: send: %v", m, err)
		}
		if res.IsError {
			t.Fatalf("%q: the send was refused: %v", m, payload(t, res))
		}
		if len(rig.provider.sent) != 1 {
			t.Errorf("%q: the message should have gone out, provider saw %d", m, len(rig.provider.sent))
		}
		if rig.queue.len() != 0 {
			t.Errorf("%q: nothing should have been queued, got %d", m, rig.queue.len())
		}
	}
}

// Everything else `hold` covers, and the two deliberate exceptions.
func TestHoldCoversTheIrreversibleAndLeavesTheRestAlone(t *testing.T) {
	held := func(t *testing.T, name string, call func(rig modeRig) (*mcp.CallToolResult, error)) {
		t.Helper()
		rig := newModeRig(t, grant.ModeHold)
		res, err := call(rig)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s was refused rather than held: %v", name, payload(t, res))
		}
		if rig.provider.touched() != 0 {
			t.Errorf("%s reached the mailbox under hold", name)
		}
		if rig.queue.len() != 1 {
			t.Errorf("%s was not queued: %d waiting", name, rig.queue.len())
		}
	}

	held(t, "mail_trash delete", func(rig modeRig) (*mcp.CallToolResult, error) {
		res, _, err := rig.tools.handleTrash(rig.ctx(), nil,
			trashArgs{IDs: []string{"acct_1:m1", "acct_1:m2"}, Action: "delete"})
		return res, err
	})
	held(t, "mail_trash trash", func(rig modeRig) (*mcp.CallToolResult, error) {
		res, _, err := rig.tools.handleTrash(rig.ctx(), nil,
			trashArgs{IDs: []string{"acct_1:m1"}, Action: "trash"})
		return res, err
	})
	held(t, "mail_filters create", func(rig modeRig) (*mcp.CallToolResult, error) {
		res, _, err := rig.tools.handleFilters(rig.ctx(), nil,
			filtersArgs{Action: "create", From: "noreply@example.com", AddLabels: []string{"TRASH"}})
		return res, err
	})
	held(t, "mail_filters delete", func(rig modeRig) (*mcp.CallToolResult, error) {
		res, _, err := rig.tools.handleFilters(rig.ctx(), nil,
			filtersArgs{Action: "delete", ID: "filter_9"})
		return res, err
	})
	held(t, "mail_settings set_vacation", func(rig modeRig) (*mcp.CallToolResult, error) {
		res, _, err := rig.tools.handleSettings(rig.ctx(), nil,
			settingsArgs{Action: "set_vacation", Enabled: true, Subject: "Away"})
		return res, err
	})

	// Restoring mail is not held: it only puts something back, and needing permission for
	// that would make the safe direction the awkward one.
	rig := newModeRig(t, grant.ModeHold)
	if _, _, err := rig.tools.handleTrash(rig.ctx(), nil,
		trashArgs{IDs: []string{"acct_1:m1"}, Action: "untrash"}); err != nil {
		t.Fatalf("untrash: %v", err)
	}
	if len(rig.provider.restored) != 1 {
		t.Error("untrash should run immediately under hold")
	}
	if rig.queue.len() != 0 {
		t.Errorf("untrash should not be queued, got %d waiting", rig.queue.len())
	}

	// Drafting is not held either. It is the thing an agent under `hold` is meant to do
	// instead, so holding it would leave the mode with no way to make progress.
	drafting := newModeRig(t, grant.ModeHold)
	res, _, err := drafting.tools.handleDraft(drafting.ctx(), nil, draftArgs{composeArgs: composeArgs{
		Account: "work", To: []addressArg{{Email: "finance@example.com"}}, Subject: "draft me",
	}})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if res.IsError {
		t.Fatalf("drafting under hold should work: %v", payload(t, res))
	}
	if drafting.queue.len() != 0 {
		t.Errorf("a draft should not be queued, got %d waiting", drafting.queue.len())
	}
}

// A grant that says hold, on a server with nowhere to hold anything, must refuse rather than
// perform. This is the "the safe path was unavailable, so we took the unsafe one" case, and
// it is the reason there is no elicitation fallback here either.
func TestHoldWithNoQueueRefusesRatherThanSends(t *testing.T) {
	provider := &sendingProvider{}
	tools := NewTools(grant.NewGate(oneMailbox{}, silentAudit{}, nil), oneProvider{provider}, oneMailbox{})
	ctx := context.WithValue(context.Background(), grantKey{}, &grant.Grant{
		ID: "g1", OwnerID: "u1", Accounts: []mail.AccountID{testAccount.ID},
		Caps: mail.NewSet(mail.AllCapabilities...), Mode: grant.ModeHold,
	})

	res, _, err := tools.handleSend(ctx, nil, sendArgsTo("finance@example.com"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !res.IsError {
		t.Fatal("a hold with nowhere to hold has to refuse the call")
	}
	if len(provider.sent) != 0 {
		t.Fatalf("the message went out anyway: %+v", provider.sent)
	}
}

// --- the mode is not the client's to change ----------------------------------------------

// An agent that can widen its own autonomy has no mode at all, so this drives every tool the
// server offers, over the real protocol, with mode-shaped arguments attached, and then asks
// the two questions that matter: is the grant still what it was, and is the server still
// refusing to send.
//
// The second question is the one with teeth. A grant object that happened not to be mutated
// proves nothing on its own — what proves it is that a send after all of that is still held.
func TestAClientCannotChangeItsOwnMode(t *testing.T) {
	rig := newModeRig(t, grant.ModeHold)

	srv := NewServer(fixedGrant{rig.grant}, rig.tools, "https://mail.example.com",
		slog.New(slog.NewTextHandler(discard{}, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: "token", base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("no tools were offered, so this test would prove nothing")
	}

	// Every name a client might reach for, on every tool the server offers. The SDK may
	// refuse an unknown property outright, and it may pass it through to a handler that
	// ignores it; either way the grant must come out the other side unchanged.
	for _, tool := range listed.Tools {
		for _, key := range []string{"mode", "grant_mode", "autonomy", "hold", "held"} {
			args := map[string]any{
				key: "unattended",
				// Enough of a real call that a handler which honoured the extra key would
				// have got far enough to act on it.
				"account": "work",
				"ids":     []string{"acct_1:m1"},
				"to":      []map[string]any{{"email": "finance@example.com"}},
				"subject": "hello", "body": "hello",
			}
			// The result is not asserted on: some of these are malformed calls and are meant
			// to be refused. What is asserted is what the server is afterwards.
			_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
		}
	}

	if rig.grant.Mode != grant.ModeHold {
		t.Fatalf("a tool call changed the grant's mode to %q", rig.grant.Mode)
	}

	res, _, err := rig.tools.handleSend(rig.ctx(), nil, sendArgsTo("finance@example.com"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.IsError {
		t.Fatalf("the send was refused rather than held: %v", payload(t, res))
	}
	if len(rig.provider.sent) != 0 {
		t.Fatalf("after all that, a send reached the provider: %+v", rig.provider.sent)
	}
	if body := payload(t, res); body["held"] != true {
		t.Fatalf("the connection stopped holding: %v", body)
	}
}

// decodeAction reads a queued action's payload the way the web side does.
func decodeAction(t *testing.T, a held.Action, into any) {
	t.Helper()
	if err := json.Unmarshal(a.Payload, into); err != nil {
		t.Fatalf("reading the queued payload: %v", err)
	}
}
