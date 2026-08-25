// Package held is the queue behind the `hold` grant mode: the privileged actions an MCP
// client asked for and this server declined to perform until its owner says so.
//
// The design question this answers is what a strict mode can actually be. Steering — the
// text of a tool's Description — is advisory: a model reads it and decides, and a model that
// decides otherwise is not stopped by it. MCP does have a way to put a question to the human
// through the client, elicitation, and it is the obvious thing to reach for here. It does not
// work for this, for two reasons and neither of them is fixable from this side.
//
// The first is negotiation. Elicitation is a capability the client declares when it
// initializes, and mailroom's transport is deliberately stateless — every request carries its
// own bearer token and no session survives between them, which is what lets a client
// reconnect or be load-balanced without re-establishing anything. The SDK synthesises the
// initialize parameters for a stateless request with no capabilities at all, so
// ServerSession.Elicit answers "client does not support elicitation" on every call this
// server will ever serve. Making it work means giving up stateless sessions for a control
// that then only exists while a connection does.
//
// The second is the answer to a client that cannot elicit, and it is the reason the first is
// not worth fixing. A control that asks the client's human, and proceeds when the client says
// there is nobody to ask, is not a control — it is a control with an opt-out that the party
// being controlled operates. Everything mailroom knows about the far end of an MCP connection
// arrived from the far end of that connection.
//
// So the question goes somewhere the client has no say in: mailroom's own web interface,
// behind the operator's session, on a page only they can reach. A held action is a complete,
// already-authorized instruction — the message with its attachments resolved, the message ids
// to delete, the filter to create — recorded so it can be carried out later unchanged. The
// tool tells the client plainly that nothing happened and that the action is waiting; the
// owner reads it and approves or discards it.
//
// One consequence is worth stating rather than discovering: a held send holds the message. A
// row here carries recipients, subject, body and attachment bytes, which is exactly what the
// audit log refuses to carry. The difference is that this is not a record of mail that exists
// — it is mail that does not exist yet and cannot be sent without being kept. Answering an
// action drops the payload in the same statement that resolves it, so what survives on the
// page afterwards is the one-line summary and nothing else.
//
// Which leaves the actions nobody answers, and they are the reason this package has a clock.
// An unanswered action used to wait forever, so the one table in this database that holds
// message bodies and attachment bytes was also the one with no retention bound on it: fifty
// per grant, at the attachment ceiling apiece, kept until somebody clicked. A held action is
// a question put to a person who is expected to answer it, so one that has gone unanswered
// for days is abandoned rather than pending, and it is treated that way — TTL past its
// creation it expires, which drops the payload exactly as answering it would and puts
// `expired` on the row where the answer would have gone. The stub survives because the
// sensitive part is the mail, not the fact that a client asked; what a stolen copy of this
// file can yield is then bounded by the TTL rather than by the age of the install.
package held

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Kind is which privileged action was asked for. It decides how the payload is read and what
// performing it means, so it is stored rather than inferred from the payload's shape.
type Kind string

const (
	KindSend      Kind = "send"
	KindSendDraft Kind = "send_draft"
	KindTrash     Kind = "trash"
	// KindModify is a label change whose effect is destruction — adding Gmail's TRASH, moving
	// into a bin folder — held for the same reason KindTrash is, because it is the same act.
	// The whole change is held, flags and ordinary labels included, so that approving it
	// performs the call the client made rather than the half of it that was privileged.
	KindModify      Kind = "modify"
	KindFilterAdd   Kind = "filter_create"
	KindFilterDrop  Kind = "filter_delete"
	KindSetVacation Kind = "set_vacation"
)

// Resolutions. A held action leaves the queue exactly once, and how it left stays on the row,
// so the page can say what became of the last thing somebody answered rather than having it
// vanish the moment it is dealt with.
const (
	Pending  = ""
	Sent     = "done"
	Declined = "discarded"
	Failed   = "failed"
	// Expired is the resolution nobody chose: the action sat unanswered past its TTL and was
	// reclaimed. It is a resolution rather than a deletion because those are two different
	// losses — dropping the row would take away the record that a client asked for this at
	// all, and that record is the cheap half. The mail is the expensive half, and it goes.
	Expired = "expired"
)

// SweepInterval is how often the reclaimer runs. It is a constant rather than a setting for
// the same reason the attachment sweeper's is: the interval decides only how long past its
// TTL a payload lingers, and nothing is served past its TTL either way — every read below is
// already cut off at it. An operator tuning retention wants the TTL.
const SweepInterval = 5 * time.Minute

var (
	// ErrNotPending is a row that has already been answered — or that belongs to somebody
	// else, which is reported the same way. Confirming that an id is real but not yours is
	// itself a disclosure.
	ErrNotPending = errors.New("no action is waiting under that id")
	// ErrFull is the per-grant cap on unanswered actions.
	ErrFull = errors.New("too many actions are already waiting for approval on this grant")
)

// MaxPending caps how many unanswered actions one grant may pile up.
//
// The cap is on attention rather than on mail. A client in `hold` mode cannot send anything,
// so no volume of held sends reaches anybody's inbox — but a confused agent that queues two
// thousand messages has buried the one the operator was going to approve, and the per-grant
// send limit cannot help because it counts sends and nothing was sent.
const MaxPending = 50

// Action is one privileged call that was not carried out.
type Action struct {
	ID        string
	OwnerID   user.ID
	GrantID   grant.ID
	AccountID mail.AccountID
	// Tool is the audit name of the call that was held — `mail.send` and the rest — so the
	// audit rows a held action writes over its life all name the same tool the client called.
	Tool string
	Kind Kind
	// Summary is one line naming what this would do, written when the action is held rather
	// than derived from the payload later. It is what the page leads with and the only part
	// of a held action the audit log ever sees.
	Summary string
	// Payload is the instruction, as JSON. Read only through the typed payloads below.
	Payload    []byte
	CreatedAt  time.Time
	ResolvedAt *time.Time
	Resolution string
	// Detail is set by the store when it loads a row for display. It is the grant's label
	// and state, which live on the grant rather than here: a held action is answered by
	// somebody deciding whether they still trust the client that asked for it.
	GrantLabel   string
	GrantRevoked bool
	Account      string
}

func (a Action) Pending() bool { return a.ResolvedAt == nil }

// SendPayload is a fully composed message, resolved down to bytes. Held before the provider
// is reached and after the attachments are gathered, so approving it cannot fail on an upload
// that has since expired or a source message that has since been deleted.
type SendPayload struct {
	Outgoing mail.Outgoing `json:"outgoing"`
	// Draft is set instead of Outgoing when the client asked to send a draft it had already
	// saved. The draft stays in the mailbox meanwhile, editable by its owner, which is the
	// one held action whose subject a person can change before approving it.
	Draft string `json:"draft,omitempty"`
}

type TrashPayload struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

// ModifyPayload is a label change held whole: the ids, both label lists, and the flags that
// travelled with them.
//
// The label ids are the ones the client named, unresolved, because that is what ApplyLabels
// takes and what the approving owner was shown. Read and Starred are pointers for the same
// reason FlagUpdate's are — a nil field is one nobody asked about, and approving a held change
// must not clear a star the client never mentioned.
type ModifyPayload struct {
	IDs     []string `json:"ids"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
	Read    *bool    `json:"read,omitempty"`
	Starred *bool    `json:"starred,omitempty"`
}

type FilterPayload struct {
	Filter   mail.Filter `json:"filter,omitempty"`
	FilterID string      `json:"filter_id,omitempty"`
}

type VacationPayload struct {
	Vacation mail.Vacation `json:"vacation"`
}

// Store is the persistence this package needs, named here rather than imported so that the
// store can hold the Action type without the two packages importing each other. Every method
// takes the owner explicitly, as everything in internal/store does.
//
// Every method that touches a pending row takes a cutoff: the oldest CreatedAt still counted
// as waiting. Rows at or before it have expired, and the cutoff is a parameter rather than a
// field on the store so that the expiry rule lives in exactly one place — this package, which
// owns the clock — instead of being a policy the persistence layer separately believes in. A
// zero cutoff expires nothing, which is how an instance that has turned retention off asks
// for the original behaviour.
type Store interface {
	HoldAction(ctx context.Context, owner user.ID, a Action) error
	PendingActions(ctx context.Context, owner user.ID, cutoff time.Time) ([]Action, error)
	RecentActions(ctx context.Context, owner user.ID, limit int) ([]Action, error)
	CountPending(ctx context.Context, owner user.ID, id grant.ID, cutoff time.Time) (int, error)
	// ClaimAction resolves a pending row and hands back what it held, in one statement. It
	// is the whole of the double-approval defence: two browser tabs pressing Approve race
	// on this UPDATE and exactly one of them gets the payload. The cutoff rides in the same
	// UPDATE, which is what makes an expired action unanswerable rather than merely hidden:
	// there is no second path to a payload, so there is no bypass to find.
	ClaimAction(ctx context.Context, owner user.ID, id, resolution string, cutoff time.Time) (Action, error)
	// MarkFailed rewrites the resolution of a row this package has already claimed.
	MarkFailed(ctx context.Context, owner user.ID, id, reason string) error
	// ExpireActions resolves every unanswered row at or before the cutoff as Expired and
	// drops its payload. Deliberately not owner-scoped: reclaiming is maintenance across the
	// whole store rather than anybody's read of their own data, exactly as sweeping blobs is.
	ExpireActions(ctx context.Context, cutoff time.Time) (int, error)
}

// ProviderFactory builds a live provider for a mailbox. The same shape internal/mcp uses, and
// satisfied by the same *app.Providers.
type ProviderFactory interface {
	For(ctx context.Context, acct mail.Account) (mail.Provider, error)
}

// Accounts resolves a mailbox by id, scoped to its owner.
type Accounts interface {
	Account(ctx context.Context, owner user.ID, id mail.AccountID) (mail.Account, error)
}

// Auditor records what happened to a held action. Same interface the gate uses, so held
// actions land in the operator's ordinary audit log rather than in a second one.
type Auditor interface {
	Record(ctx context.Context, entry grant.Audit) error
}

// Queue is the held-action queue: the MCP side puts actions in, the web side takes them out,
// and anything neither side dealt with in ttl leaves on its own.
type Queue struct {
	store     Store
	providers ProviderFactory
	accounts  Accounts
	auditor   Auditor
	// ttl is how long an unanswered action stays answerable, and how long its payload stays
	// on disk. Zero turns expiry off entirely.
	ttl time.Duration
	now func() time.Time
}

// New builds the queue. The TTL is an argument rather than a setter because it is a retention
// policy over message bodies, and a control that has to be remembered by each caller is one
// that will eventually be forgotten by one of them.
func New(s Store, providers ProviderFactory, accounts Accounts, auditor Auditor, ttl time.Duration) *Queue {
	return &Queue{
		store: s, providers: providers, accounts: accounts, auditor: auditor,
		ttl: ttl, now: time.Now,
	}
}

// cutoff is the oldest CreatedAt still counted as waiting. Zero when retention is off, which
// every query below reads as "expire nothing".
func (q *Queue) cutoff() time.Time {
	if q.ttl <= 0 {
		return time.Time{}
	}
	return q.now().Add(-q.ttl)
}

// TTL reports how long an unanswered action lasts, so the page that draws the queue can say
// so rather than leaving it to be found out.
func (q *Queue) TTL() time.Duration { return q.ttl }

// Sweep resolves everything that sat unanswered past its TTL, dropping the payloads.
func (q *Queue) Sweep(ctx context.Context) (int, error) {
	if q.ttl <= 0 {
		return 0, nil
	}
	return q.store.ExpireActions(ctx, q.cutoff())
}

// SweepEvery runs the reclaimer until the context ends, starting with one pass immediately —
// a restart after downtime is exactly when the queue is holding mail that should already be
// gone. The same shape, and for the same reason, as the attachment store's sweeper.
func (q *Queue) SweepEvery(ctx context.Context, every time.Duration, log *slog.Logger) {
	if q.ttl <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := q.Sweep(ctx); err != nil {
			log.Warn("could not expire held actions", "err", err)
		} else if n > 0 {
			log.Info("expired unanswered held actions", "count", n, "ttl", q.ttl.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Hold records an action instead of performing it, and answers with the row it wrote.
//
// The caller has already authorized the call against the grant. That ordering matters: a
// client asking for something its grant does not cover is refused rather than queued, so the
// queue is a list of things that would have happened, not a list of things that were asked
// for.
func (q *Queue) Hold(ctx context.Context, g *grant.Grant, acct mail.Account, tool string, kind Kind, summary string, payload any) (Action, error) {
	// Counted at the cutoff, so a grant whose queue is fifty abandoned actions is not a grant
	// that can never queue another. The cap is on somebody's attention, and nothing expired is
	// waiting for it.
	n, err := q.store.CountPending(ctx, g.OwnerID, g.ID, q.cutoff())
	if err != nil {
		// Fail closed. If the queue depth cannot be read the cap cannot be enforced, and an
		// unenforceable cap should stop the call rather than wave it through — the same
		// choice the send limit makes for the same reason.
		return Action{}, fmt.Errorf("could not check how much is already waiting, so nothing was queued: %w", err)
	}
	if n >= MaxPending {
		return Action{}, ErrFull
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Action{}, fmt.Errorf("could not record what was asked for: %w", err)
	}

	a := Action{
		ID: ids.New("held"), OwnerID: g.OwnerID, GrantID: g.ID, AccountID: acct.ID,
		Tool: tool, Kind: kind, Summary: summary, Payload: body, CreatedAt: q.now(),
	}
	if err := q.store.HoldAction(ctx, g.OwnerID, a); err != nil {
		return Action{}, err
	}
	return a, nil
}

// Pending is what is still waiting for this owner, reclaiming anything that has expired on
// the way past.
//
// The reclaim is the same choice blob.Ref makes: a payload should not survive its TTL merely
// because the page was opened before the next sweep. Its error is dropped deliberately — the
// listing below is already cut off at the same instant, so a failed reclaim shows nobody a
// stale row, and refusing to draw the queue because a cleanup did not run would hide the
// actions somebody came here to answer.
func (q *Queue) Pending(ctx context.Context, owner user.ID) ([]Action, error) {
	_, _ = q.Sweep(ctx)
	return q.store.PendingActions(ctx, owner, q.cutoff())
}

func (q *Queue) Recent(ctx context.Context, owner user.ID, limit int) ([]Action, error) {
	return q.store.RecentActions(ctx, owner, limit)
}

func (q *Queue) Count(ctx context.Context, owner user.ID) (int, error) {
	pending, err := q.store.PendingActions(ctx, owner, q.cutoff())
	return len(pending), err
}

// Decline discards an action without performing it.
func (q *Queue) Decline(ctx context.Context, owner user.ID, id string) (Action, error) {
	a, err := q.store.ClaimAction(ctx, owner, id, Declined, q.cutoff())
	if err != nil {
		return Action{}, err
	}
	q.record(ctx, a, "declined")
	return a, nil
}

// Approve performs a held action.
//
// An action past its TTL is not approvable here, and the enforcement is not a check in this
// function: the cutoff travels into the conditional UPDATE that claims the row, so an expired
// action matches nothing and comes back as ErrNotPending like any other row that has already
// left the queue. There is no path to a payload that does not go through that statement.
//
// The row is claimed first and performed second, which is the wrong way round for reporting
// and the right way round for mail. Claiming first means two tabs, or a double submit, race
// on one UPDATE and only the winner holds the payload — so a message cannot go out twice. It
// costs the other case: an action whose provider call then fails is already out of the queue,
// and is marked `failed` with the reason rather than returning to wait. That is deliberate.
// Sending twice is unrecoverable and confusing to the recipient; a failed row the operator
// can see, and ask the client to compose again, is neither.
func (q *Queue) Approve(ctx context.Context, owner user.ID, id string) (Action, error) {
	a, err := q.store.ClaimAction(ctx, owner, id, Sent, q.cutoff())
	if err != nil {
		return Action{}, err
	}

	if err := q.perform(ctx, a); err != nil {
		a.Resolution = Failed
		if mark := q.store.MarkFailed(ctx, owner, id, err.Error()); mark != nil {
			// The action did not happen and the row still says it did. Worth a loud audit
			// row, since the page will now be wrong about it.
			q.record(ctx, a, "failed_unrecorded")
			return a, fmt.Errorf("%w (and the queue could not be updated: %v)", err, mark)
		}
		q.record(ctx, a, mail.Code(err))
		return a, err
	}

	q.record(ctx, a, "ok")
	return a, nil
}

// perform carries out an action against the mailbox it names.
//
// The mailbox is re-resolved from the store, scoped to the owner on the row, rather than
// trusted from anything the form posted: an id typed at this endpoint reaches no further than
// one clicked on the page.
func (q *Queue) perform(ctx context.Context, a Action) error {
	acct, err := q.accounts.Account(ctx, a.OwnerID, a.AccountID)
	if err != nil {
		return fmt.Errorf("the mailbox this was for is no longer linked")
	}
	p, err := q.providers.For(ctx, acct)
	if err != nil {
		return err
	}

	switch a.Kind {
	case KindSend:
		var payload SendPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		writer, ok := p.(mail.MessageWriter)
		if !ok {
			return unsupported(p, acct, mail.CapSend)
		}
		_, err := writer.Send(ctx, payload.Outgoing)
		return err

	case KindSendDraft:
		var payload SendPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		id, err := mail.ParseScopedID(payload.Draft)
		if err != nil {
			return err
		}
		drafts, ok := p.(mail.DraftManager)
		if !ok {
			return unsupported(p, acct, mail.CapSend)
		}
		_, err = drafts.SendDraft(ctx, id)
		return err

	case KindTrash:
		var payload TrashPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		destroyer, ok := p.(mail.Destroyer)
		if !ok {
			return unsupported(p, acct, mail.CapDestructive)
		}
		ids, err := scopedIDs(payload.IDs)
		if err != nil {
			return err
		}
		switch payload.Action {
		case "delete":
			return destroyer.Delete(ctx, ids)
		default:
			return destroyer.Trash(ctx, ids)
		}

	case KindModify:
		var payload ModifyPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		labels, ok := p.(mail.LabelManager)
		if !ok {
			return unsupported(p, acct, mail.CapLabels)
		}
		ids, err := scopedIDs(payload.IDs)
		if err != nil {
			return err
		}
		// Flags before labels, in the order the tool performs them: the label half is the one
		// that moves the message, and a mailbox left with the flags written and the move
		// refused is closer to what was asked for than the reverse.
		update := mail.FlagUpdate{Read: payload.Read, Starred: payload.Starred}
		if !update.Empty() {
			if err := labels.SetFlags(ctx, ids, update); err != nil {
				return err
			}
		}
		return labels.ApplyLabels(ctx, ids, labelIDs(payload.Add), labelIDs(payload.Remove))

	case KindFilterAdd:
		var payload FilterPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		manager, ok := p.(mail.FilterManager)
		if !ok {
			return unsupported(p, acct, mail.CapFilters)
		}
		_, err := manager.CreateFilter(ctx, payload.Filter)
		return err

	case KindFilterDrop:
		var payload FilterPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		manager, ok := p.(mail.FilterManager)
		if !ok {
			return unsupported(p, acct, mail.CapFilters)
		}
		return manager.DeleteFilter(ctx, payload.FilterID)

	case KindSetVacation:
		var payload VacationPayload
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			return err
		}
		manager, ok := p.(mail.SettingsManager)
		if !ok {
			return unsupported(p, acct, mail.CapSettings)
		}
		return manager.SetVacation(ctx, payload.Vacation)
	}

	// A kind this build does not know is a row written by a newer one. Refusing is the only
	// safe answer: guessing what it meant is guessing with somebody's mail.
	return fmt.Errorf("this build does not know how to perform a %q action", a.Kind)
}

func unsupported(p mail.Provider, acct mail.Account, c mail.Capability) error {
	return &mail.UnsupportedError{
		Provider: p.ID(), Account: acct.Alias, Address: acct.Address, Capability: c,
	}
}

func labelIDs(in []string) []mail.LabelID {
	out := make([]mail.LabelID, len(in))
	for i, s := range in {
		out[i] = mail.LabelID(s)
	}
	return out
}

func scopedIDs(in []string) ([]mail.ScopedID, error) {
	out := make([]mail.ScopedID, 0, len(in))
	for _, s := range in {
		id, err := mail.ParseScopedID(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// record writes what became of a held action to the ordinary audit log.
//
// Failures are logged nowhere and returned nowhere: by the time this runs the mail has either
// gone or been discarded, so refusing prevents nothing, and an error about bookkeeping would
// displace the one the operator asked about. That is the same split grant.Gate makes between
// a read it can still withhold and a change it cannot.
func (q *Queue) record(ctx context.Context, a Action, outcome string) {
	if q.auditor == nil {
		return
	}
	_ = q.auditor.Record(ctx, grant.Audit{
		OwnerID: a.OwnerID, GrantID: a.GrantID, AccountID: a.AccountID,
		Tool: a.Tool, Outcome: outcome, At: q.now(),
	})
}

// Describe names what a held send would do, for the one line the queue page and the audit log
// both lead with. Recipients and subject only: the body is on the page, under the summary,
// where somebody has chosen to read it.
func DescribeSend(out mail.Outgoing) string {
	to := make([]string, 0, len(out.To))
	for _, a := range out.To {
		to = append(to, a.Email)
	}
	subject := out.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	if len(to) == 0 {
		return "send " + subject
	}
	return "send " + subject + " to " + strings.Join(to, ", ")
}
