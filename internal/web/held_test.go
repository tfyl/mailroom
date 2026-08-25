package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// The half of a grant's mode that is a control rather than a suggestion, driven through the
// real handlers and the real store.
//
// The question these answer is the one a steering-only feature cannot: not "was the client
// told to ask" but "did the mail go out". So every one of them ends by looking at what the
// provider was handed.

// recordingMailbox stands in for a mail server. Approving a held action has to reach it;
// declining one has to not.
type recordingMailbox struct {
	sent     []mail.Outgoing
	vacation []mail.Vacation
}

func (m *recordingMailbox) ID() mail.ProviderID    { return mail.ProviderIMAP }
func (m *recordingMailbox) Capabilities() mail.Set { return mail.DerivedCapabilities(m) }
func (m *recordingMailbox) Quirks() []mail.Quirk   { return nil }

func (m *recordingMailbox) Send(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	m.sent = append(m.sent, out)
	return mail.ScopedID{Account: out.Account, Native: "sent_1"}, nil
}

func (m *recordingMailbox) ListSendAs(context.Context) ([]mail.SendAs, error) { return nil, nil }
func (m *recordingMailbox) GetVacation(context.Context) (mail.Vacation, error) {
	return mail.Vacation{}, nil
}

func (m *recordingMailbox) SetVacation(_ context.Context, v mail.Vacation) error {
	m.vacation = append(m.vacation, v)
	return nil
}

type oneMailbox struct{ p *recordingMailbox }

func (o oneMailbox) For(context.Context, mail.Account) (mail.Provider, error) { return o.p, nil }

// heldTestTTL is long enough that nothing in this file expires by accident. Retention itself
// is exercised in internal/held and internal/store, against the clock rather than the wall.
const heldTestTTL = time.Hour

type heldRig struct {
	s       *Server
	db      *store.Store
	mailbox *recordingMailbox
	ada     user.User
	bob     user.User
	grant   grant.ID
}

// newHeldRig builds the product with a mail server that records rather than sends. It is the
// same store, the same queue type and the same handlers as production; only the far end of
// the provider is replaced, because the assertion is about what reaches it.
func newHeldRig(t *testing.T) heldRig {
	t.Helper()
	ctx := context.Background()

	s, db := testServer(t, signup.Policy{Mode: signup.Open})
	signInAs(s, "ada", "")
	signInAs(s, "bob", "")

	rig := heldRig{s: s, db: db, mailbox: &recordingMailbox{}}
	s.holds = held.New(db, oneMailbox{rig.mailbox}, db, db, heldTestTTL)

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		switch u.Subject {
		case "ada":
			rig.ada = u
		case "bob":
			rig.bob = u
		}
	}

	link := func(owner user.User, id, alias string) {
		t.Helper()
		err := db.LinkAccount(ctx, owner.ID, mail.Account{
			ID: mail.AccountID(id), Alias: alias, Address: alias + "@example.com",
			Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
		}, "sealed", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	link(rig.ada, "acct_ada_work", "ada-work")
	link(rig.bob, "acct_bob_work", "bob-work")

	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	rig.grant = "grant_held"
	err = db.CreateGrant(ctx, &grant.Grant{
		ID: rig.grant, OwnerID: rig.ada.ID, ClientID: "client_1", Label: "Claude",
		Accounts: []mail.AccountID{"acct_ada_work"},
		Caps:     mail.NewSet(mail.CapRead, mail.CapSend, mail.CapSettings),
		Mode:     grant.ModeHold,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rig
}

// queueSend puts a composed message in the queue the way internal/mcp does when a grant's
// mode says a send must wait.
func (rig heldRig) queueSend(t *testing.T, to, subject string) held.Action {
	t.Helper()
	g, err := rig.db.Grant(context.Background(), rig.grant)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := rig.db.Account(context.Background(), rig.ada.ID, "acct_ada_work")
	if err != nil {
		t.Fatal(err)
	}
	out := mail.Outgoing{
		Account: acct.ID,
		To:      []mail.Address{{Email: to}},
		Subject: subject,
		Body:    mail.Body{Text: "The numbers are attached."},
	}
	action, err := rig.s.holds.Hold(context.Background(), g, acct, "mail.send",
		held.KindSend, held.DescribeSend(out), held.SendPayload{Outgoing: out})
	if err != nil {
		t.Fatal(err)
	}
	return action
}

func (rig heldRig) post(t *testing.T, as user.User, path, id string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"id": {id}}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	switch path {
	case "/held/approve":
		rig.s.approveHeld(rec, r)
	default:
		rig.s.declineHeld(rec, r)
	}
	return rec
}

func (rig heldRig) page(t *testing.T, as user.User) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/held", nil)
	r = r.WithContext(user.NewContext(r.Context(), as))
	rec := httptest.NewRecorder()
	rig.s.heldQueue(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /held: %d", rec.Code)
	}
	return rec.Body.String()
}

func (rig heldRig) pending(t *testing.T, as user.User) []held.Action {
	t.Helper()
	out, err := rig.s.holds.Pending(context.Background(), as.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Approving in the UI is what actually sends the message. Until somebody presses it, the
// message exists only as a row.
func TestApprovingAHeldSendIsWhatSendsIt(t *testing.T) {
	rig := newHeldRig(t)
	action := rig.queueSend(t, "finance@example.com", "the quarterly numbers")

	if len(rig.mailbox.sent) != 0 {
		t.Fatalf("queueing sent the message: %+v", rig.mailbox.sent)
	}
	if body := rig.page(t, rig.ada); !strings.Contains(body, "the quarterly numbers") ||
		!strings.Contains(body, "finance@example.com") {
		t.Error("the page should show the message so it can be read before it is approved")
	}

	rec := rig.post(t, rig.ada, "/held/approve", action.ID)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: want a redirect, got %d: %s", rec.Code, rec.Body)
	}

	if len(rig.mailbox.sent) != 1 {
		t.Fatalf("approving did not send the message: %+v", rig.mailbox.sent)
	}
	if got := rig.mailbox.sent[0]; got.Subject != "the quarterly numbers" ||
		len(got.To) != 1 || got.To[0].Email != "finance@example.com" {
		t.Errorf("what was sent is not what was queued: %+v", got)
	}
	if n := len(rig.pending(t, rig.ada)); n != 0 {
		t.Errorf("the action should have left the queue, %d still waiting", n)
	}

	// What survives is the line it was listed by, and none of the message. This is the one
	// table in the database that holds mail bodies, and it holds them only for as long as the
	// instruction is waiting to be carried out.
	answered, err := rig.s.holds.Recent(context.Background(), rig.ada.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(answered) != 1 {
		t.Fatalf("want the answered action kept, got %d", len(answered))
	}
	if len(answered[0].Payload) != 0 {
		t.Errorf("the message is still in the database after it was sent: %s", answered[0].Payload)
	}
	if answered[0].Summary == "" {
		t.Error("and the line it was listed by should have been kept")
	}
}

// Declining is the other half, and the one a queue is useless without.
func TestDecliningAHeldSendDoesNotSendIt(t *testing.T) {
	rig := newHeldRig(t)
	action := rig.queueSend(t, "everyone@example.com", "a mistake")

	rec := rig.post(t, rig.ada, "/held/decline", action.ID)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("decline: want a redirect, got %d: %s", rec.Code, rec.Body)
	}

	if len(rig.mailbox.sent) != 0 {
		t.Fatalf("discarding sent the message: %+v", rig.mailbox.sent)
	}
	if n := len(rig.pending(t, rig.ada)); n != 0 {
		t.Errorf("the action should have left the queue, %d still waiting", n)
	}

	// And it cannot be brought back by pressing Approve on the same id afterwards.
	if again := rig.post(t, rig.ada, "/held/approve", action.ID); again.Code != http.StatusNotFound {
		t.Errorf("a discarded action was approvable afterwards: %d", again.Code)
	}
	if len(rig.mailbox.sent) != 0 {
		t.Fatalf("a discarded message was sent on a second attempt: %+v", rig.mailbox.sent)
	}
}

// A queued message is somebody's mail. On a shared instance it must be invisible and
// untouchable to everybody else, and an id that leaks or is guessed must reach nothing.
func TestAHeldActionIsOnlyItsOwnersToSee(t *testing.T) {
	rig := newHeldRig(t)
	action := rig.queueSend(t, "finance@example.com", "the quarterly numbers")

	if body := rig.page(t, rig.bob); strings.Contains(body, "the quarterly numbers") ||
		strings.Contains(body, "finance@example.com") {
		t.Error("another user's held message is on bob's page")
	}
	if n := len(rig.pending(t, rig.bob)); n != 0 {
		t.Errorf("bob has %d of ada's actions waiting", n)
	}

	// Reported as missing rather than forbidden: confirming that an id is real but not yours
	// is itself a disclosure.
	if rec := rig.post(t, rig.bob, "/held/approve", action.ID); rec.Code != http.StatusNotFound {
		t.Errorf("bob could reach ada's held action: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.post(t, rig.bob, "/held/decline", action.ID); rec.Code != http.StatusNotFound {
		t.Errorf("bob could discard ada's held action: %d %s", rec.Code, rec.Body)
	}

	if len(rig.mailbox.sent) != 0 {
		t.Fatalf("bob's attempts sent the message: %+v", rig.mailbox.sent)
	}
	if n := len(rig.pending(t, rig.ada)); n != 1 {
		t.Errorf("ada's action should still be waiting, %d found", n)
	}
}

// A double submit, a second tab, or a form resubmitted from the browser's history all arrive
// as a second Approve on the same id. Sending the message twice is the one outcome nothing
// can undo, so the second one has to find nothing waiting.
func TestApprovingTwiceSendsOnce(t *testing.T) {
	rig := newHeldRig(t)
	action := rig.queueSend(t, "finance@example.com", "the quarterly numbers")

	if rec := rig.post(t, rig.ada, "/held/approve", action.ID); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first approval failed: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.post(t, rig.ada, "/held/approve", action.ID); rec.Code != http.StatusNotFound {
		t.Errorf("the second approval should have found nothing waiting, got %d", rec.Code)
	}
	if len(rig.mailbox.sent) != 1 {
		t.Fatalf("the message went out %d times", len(rig.mailbox.sent))
	}
}

// A held action's whole life is in the audit log: the call that was held, and what its owner
// decided. Reading it afterwards has to show both, and the hold must not read as a send.
func TestTheAuditLogRecordsAHeldActionAndItsAnswer(t *testing.T) {
	rig := newHeldRig(t)
	sent := rig.queueSend(t, "finance@example.com", "the quarterly numbers")
	discarded := rig.queueSend(t, "everyone@example.com", "a mistake")

	rig.post(t, rig.ada, "/held/approve", sent.ID)
	rig.post(t, rig.ada, "/held/decline", discarded.ID)

	entries, err := rig.db.RecentAudit(context.Background(), rig.ada.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for _, e := range entries {
		if e.Tool == "mail.send" {
			outcomes[e.Outcome]++
		}
	}
	if outcomes["ok"] != 1 {
		t.Errorf("want exactly one send recorded as ok, got %d: %v", outcomes["ok"], outcomes)
	}
	if outcomes["declined"] != 1 {
		t.Errorf("want the discarded action recorded, got %v", outcomes)
	}

	// The audit page must not count a hold as a refusal. Nothing was turned away — the call
	// was recorded and answered somewhere else.
	r := httptest.NewRequest(http.MethodGet, "/audit", nil)
	r = r.WithContext(user.NewContext(r.Context(), rig.ada))
	rec := httptest.NewRecorder()
	rig.s.audit(rec, r)
	if strings.Contains(rec.Body.String(), "1 refused") {
		t.Error("a held action is being counted as a refusal on the audit page")
	}
}

// Loosening a grant off `hold` decides what happens next. It must not release what is already
// waiting: those were queued for a person to decide about, and an edit to a setting is not
// that person deciding about them.
func TestLooseningTheModeDoesNotReleaseWhatIsAlreadyWaiting(t *testing.T) {
	rig := newHeldRig(t)
	rig.queueSend(t, "finance@example.com", "the quarterly numbers")

	err := rig.db.EditGrant(context.Background(), rig.ada.ID, rig.grant,
		[]mail.AccountID{"acct_ada_work"},
		mail.NewSet(mail.CapRead, mail.CapSend, mail.CapSettings),
		grant.ModeUnattended, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(rig.mailbox.sent) != 0 {
		t.Fatalf("changing the mode sent what was waiting: %+v", rig.mailbox.sent)
	}
	if n := len(rig.pending(t, rig.ada)); n != 1 {
		t.Errorf("the queued action should still be waiting, %d found", n)
	}
}

// queueSendAged puts a message in the queue as if it had been sitting there, by writing the
// row the way internal/held writes it and dating it back. The store takes the action's own
// CreatedAt, so this is the real INSERT rather than a stand-in — only the clock is chosen.
func (rig heldRig) queueSendAged(t *testing.T, to, subject string, age time.Duration) held.Action {
	t.Helper()
	out := mail.Outgoing{
		Account: "acct_ada_work",
		To:      []mail.Address{{Email: to}},
		Subject: subject,
		Body:    mail.Body{Text: "The numbers are attached."},
	}
	payload, err := json.Marshal(held.SendPayload{Outgoing: out})
	if err != nil {
		t.Fatal(err)
	}
	a := held.Action{
		ID: "held_aged_" + subject, OwnerID: rig.ada.ID, GrantID: rig.grant,
		AccountID: "acct_ada_work", Tool: "mail.send", Kind: held.KindSend,
		Summary: held.DescribeSend(out), Payload: payload,
		CreatedAt: time.Now().Add(-age),
	}
	if err := rig.db.HoldAction(context.Background(), rig.ada.ID, a); err != nil {
		t.Fatal(err)
	}
	return a
}

// The whole of the retention bound, through the real handlers: an action nobody answered
// stops being answerable, and the message it was holding is gone from the database.
//
// The assertion that matters is the last one. A queued send is a whole message — recipients,
// body, attachment bytes — sitting unencrypted in the one table that stores such a thing, and
// until this it sat there for as long as the install lived.
func TestAnActionNobodyAnsweredExpiresAndCannotBeApproved(t *testing.T) {
	rig := newHeldRig(t)
	stale := rig.queueSendAged(t, "finance@example.com", "abandoned", 96*time.Hour)
	fresh := rig.queueSend(t, "finance@example.com", "this morning")

	t.Run("the page offers only what is still waiting", func(t *testing.T) {
		// Matched on the id, which only an approve or discard form carries. The expired
		// action's summary is still on the page — in the closed list, which is where it
		// belongs and is not an offer to send anything.
		body := rig.page(t, rig.ada)
		if strings.Contains(body, `value="`+stale.ID+`"`) {
			t.Error("an expired action is still being offered for approval")
		}
		if !strings.Contains(body, `value="`+fresh.ID+`"`) {
			t.Error("an action inside its TTL should still be answerable on the page")
		}
		if !strings.Contains(body, "expires") && !strings.Contains(body, "Expires") {
			t.Error("the page should say how long a held action waits, not leave it to be found out")
		}
	})

	t.Run("approving it is refused", func(t *testing.T) {
		rec := rig.post(t, rig.ada, "/held/approve", stale.ID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("approving an expired action answered %d, want 404", rec.Code)
		}
		if len(rig.mailbox.sent) != 0 {
			t.Fatalf("an expired action reached the mail server: %+v", rig.mailbox.sent)
		}
	})

	t.Run("discarding it is refused too", func(t *testing.T) {
		if rec := rig.post(t, rig.ada, "/held/decline", stale.ID); rec.Code != http.StatusNotFound {
			t.Fatalf("discarding an expired action answered %d, want 404", rec.Code)
		}
	})

	t.Run("the message it was holding is gone", func(t *testing.T) {
		recent, err := rig.s.holds.Recent(context.Background(), rig.ada.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, a := range recent {
			if a.ID != stale.ID {
				continue
			}
			found = true
			if a.Resolution != held.Expired {
				t.Errorf("resolution is %q, want %q", a.Resolution, held.Expired)
			}
			if len(a.Payload) != 0 {
				t.Errorf("the message survived expiry: %q", a.Payload)
			}
			if a.Summary == "" {
				t.Error("the record of what was asked for should survive")
			}
		}
		if !found {
			t.Error("an expired action should stay in the history as a stub, not vanish")
		}
	})

	t.Run("what was queued today is untouched", func(t *testing.T) {
		if rec := rig.post(t, rig.ada, "/held/approve", fresh.ID); rec.Code != http.StatusSeeOther {
			t.Fatalf("approving a fresh action answered %d, want 303", rec.Code)
		}
		if len(rig.mailbox.sent) != 1 {
			t.Fatalf("the mail server saw %d messages, want 1", len(rig.mailbox.sent))
		}
	})
}

// Opening the page is what reclaimed the message above, before any sweeper ran. Expiry on
// read matters because the sweeper's interval is not a promise: a payload must not outlive
// its TTL merely because somebody looked at the queue between two passes.
func TestOpeningThePageReclaimsWhatHasExpired(t *testing.T) {
	rig := newHeldRig(t)
	stale := rig.queueSendAged(t, "finance@example.com", "abandoned", 96*time.Hour)

	rig.page(t, rig.ada)

	recent, err := rig.s.holds.Recent(context.Background(), rig.ada.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].ID != stale.ID || recent[0].Resolution != held.Expired {
		t.Fatalf("drawing the page did not reclaim the expired action: %+v", recent)
	}
}
