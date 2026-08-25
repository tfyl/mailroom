package grant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Resolver looks up the accounts a grant may reach. Implemented by the store.
//
// Every lookup is scoped to an owner. The gate passes the grant's own owner, so a grant can
// only ever resolve mailboxes belonging to the user who approved it — even if its stored
// account list were somehow to name one that does not.
type Resolver interface {
	Account(ctx context.Context, owner user.ID, id mail.AccountID) (mail.Account, error)
	AccountByAlias(ctx context.Context, owner user.ID, alias string) (mail.Account, error)
	AccountByAddress(ctx context.Context, owner user.ID, address string) (mail.Account, error)
}

// StatusWriter marks a mailbox as no longer usable. Separate from Resolver because finding a
// mailbox and changing one are different powers, and the gate needs the first far more often
// than the second.
type StatusWriter interface {
	SetAccountStatus(ctx context.Context, owner user.ID, id mail.AccountID, status mail.AccountStatus) error
}

// Auditor records what a grant did. Never a message body — the Audit it takes has no field
// that would hold one, and docs/security.md says where that line falls and why.
type Auditor interface {
	Record(ctx context.Context, entry Audit) error
}

// Gate resolves the accounts a call may touch and enforces the grant against each. Every
// path from a tool to a provider runs through here; there is deliberately no other way to
// obtain an account.
type Gate struct {
	resolver Resolver
	auditor  Auditor
	status   StatusWriter
	now      func() time.Time
}

func NewGate(r Resolver, a Auditor, s StatusWriter) *Gate {
	return &Gate{resolver: r, auditor: a, status: s, now: time.Now}
}

// Resolve turns the caller's `account` parameter into the concrete accounts a call may use.
//
// The selector accepts an alias, an address, or a list of either. An empty selector means
// every account the grant names — omission is the only way to say "everything I may see",
// because a literal "all" would silently widen as new mailboxes are linked.
//
// Naming an account outside the grant is an error rather than a silent drop: a caller that
// asked for two mailboxes and received results from one, with no error, will report to its
// user as though it searched both.
//
// A refusal is written to the audit log here rather than at each tool, which is why the tool
// name is a parameter. Every refusal this function returns is a call that touched no mailbox
// and produced no row at all until now — so the one page an operator opens to find out what a
// client was turned away from showed provider failures and nothing else.
func (g *Gate) Resolve(ctx context.Context, gr *Grant, tool string, selector []string, c mail.Capability) ([]mail.Account, error) {
	now := g.now()
	if err := gr.Valid(now); err != nil {
		g.refuse(ctx, gr, Audit{Tool: tool, Capability: c}, err)
		return nil, err
	}

	var candidates []mail.Account
	if len(selector) == 0 {
		for _, id := range gr.Accounts {
			acct, err := g.resolver.Account(ctx, gr.OwnerID, id)
			if err != nil {
				// A grant referencing an account that no longer exists is not fatal: the
				// operator deleted a mailbox and the remaining scope is still meaningful.
				continue
			}
			candidates = append(candidates, acct)
		}
		if len(candidates) == 0 {
			g.refuse(ctx, gr, Audit{Tool: tool, Capability: c}, ErrNoScope)
			return nil, ErrNoScope
		}
	} else {
		for _, sel := range selector {
			acct, err := g.lookup(ctx, gr.OwnerID, sel)
			if err != nil {
				// The selector is not echoed into the audit row. It is caller-supplied text
				// that resolved to no mailbox of this owner's, and a log that stored it would
				// let a client write whatever it liked into the operator's own page.
				unknown := fmt.Errorf("unknown account %q", sel)
				g.refuse(ctx, gr, Audit{Tool: tool, Capability: c}, unknown)
				return nil, unknown
			}
			candidates = append(candidates, acct)
		}
	}

	// Authorize every candidate. An explicit selector fails loudly on the first refusal; an
	// implicit fan-out skips accounts that cannot serve this capability, since the caller
	// asked for "everything I may see" rather than for that account specifically.
	//
	// Only the loud half is recorded. A skip inside an implicit fan-out did not refuse the
	// call — the call goes on to succeed against the mailboxes that do serve it — and a row
	// per skipped mailbox would put a refusal on the page for every search a grant with two
	// capabilities and five mailboxes ever ran.
	explicit := len(selector) > 0
	var allowed []mail.Account
	for _, acct := range candidates {
		if err := gr.Authorize(now, acct, c); err != nil {
			if explicit {
				g.refuse(ctx, gr, Audit{AccountID: acct.ID, Tool: tool, Capability: c}, err)
				return nil, err
			}
			continue
		}
		allowed = append(allowed, acct)
	}

	if len(allowed) == 0 {
		err := &mail.ScopeError{Account: "any linked account", Capability: c, Held: gr.Caps}
		g.refuse(ctx, gr, Audit{Tool: tool, Capability: c}, err)
		return nil, err
	}
	return allowed, nil
}

// ResolveOne resolves a single account for an operation that names a specific message or
// thread. The account comes from the identifier itself, so there is nothing to fan out.
func (g *Gate) ResolveOne(ctx context.Context, gr *Grant, tool string, id mail.ScopedID, c mail.Capability) (mail.Account, error) {
	acct, err := g.resolveOne(ctx, gr, id, c)
	if err != nil {
		e := Audit{Tool: tool, Capability: c}
		// The account is named only when it resolved, which means the owner-scoped lookup
		// found it and this operator owns it. An id that resolved to nothing is a string the
		// caller chose, and writing it into the account column would let a client put another
		// user's account id — and so, through the page's join, another user's alias — into
		// this operator's log.
		if acct.ID != "" {
			e.AccountID = acct.ID
		}
		g.refuse(ctx, gr, e, err)
		return mail.Account{}, err
	}
	return acct, nil
}

// ResolveInBatch is ResolveOne without the audit row, for a tool authorizing a list of ids.
//
// The check is identical and there is no way to skip it; only the recording differs. A batch
// of fifty ids that a grant cannot reach is one refused call, not fifty, and the tool writes
// the single row that says so with the count on it. Recording per id would let one malformed
// call from a confused client fill a page an operator reads by scanning it.
func (g *Gate) ResolveInBatch(ctx context.Context, gr *Grant, id mail.ScopedID, c mail.Capability) (mail.Account, error) {
	return g.resolveOne(ctx, gr, id, c)
}

// resolveOne returns the account alongside the error when the mailbox itself resolved, so
// callers can tell "this grant may not touch that mailbox" from "no such mailbox".
func (g *Gate) resolveOne(ctx context.Context, gr *Grant, id mail.ScopedID, c mail.Capability) (mail.Account, error) {
	// Scoped to the grant's owner: an id naming somebody else's mailbox resolves to nothing,
	// so guessing or replaying one gets a not-found rather than access.
	acct, err := g.resolver.Account(ctx, gr.OwnerID, id.Account)
	if err != nil {
		return mail.Account{}, mail.ErrNotFound
	}
	if err := gr.Authorize(g.now(), acct, c); err != nil {
		return acct, err
	}
	return acct, nil
}

func (g *Gate) lookup(ctx context.Context, owner user.ID, sel string) (mail.Account, error) {
	if acct, err := g.resolver.AccountByAlias(ctx, owner, sel); err == nil {
		return acct, nil
	}
	return g.resolver.AccountByAddress(ctx, owner, sel)
}

// Record writes an audit entry, and reports whether it was written.
//
// The failure is the caller's to act on, and there are two right answers rather than one.
// Where the result of a call has not reached the client yet — every read — a row that cannot
// be written refuses the call: the answer is withheld, and "no mailbox is read unrecorded"
// stays true. Where the mailbox has already changed, refusing prevents nothing. The message
// is sent, the accountability is already lost, and an error would report a failure that did
// not happen and invite a retry that sends it twice; those calls say in their result that
// the change went unrecorded. Both halves live in internal/mcp as auditRead and auditChange.
//
// The owner, the grant and the time come from here rather than from the caller, so no call
// site can write a row against a grant other than the one it is serving.
func (g *Gate) Record(ctx context.Context, gr *Grant, e Audit) error {
	if g.auditor == nil {
		return nil
	}
	e.OwnerID, e.GrantID, e.At = gr.OwnerID, gr.ID, g.now()
	return g.auditor.Record(ctx, e)
}

// refuse records a call the gate turned away.
//
// The write failure is deliberately dropped. Nothing was disclosed — the call reached no
// mailbox — so there is no answer to withhold, and replacing the refusal the client asked
// about with one about bookkeeping would hide the reason it was actually turned away.
func (g *Gate) refuse(ctx context.Context, gr *Grant, e Audit, err error) {
	e.Outcome, e.Reason = RefusedAs(err), err.Error()
	if recErr := g.Record(ctx, gr, e); recErr != nil {
		slog.Default().Warn("could not record a refused call in the audit log",
			"grant", gr.ID, "tool", e.Tool, "err", recErr)
	}
}

// RefusedAs classifies a refusal for the audit log's outcome column.
//
// The three grant states get names of their own because "error" is what they used to be, and
// an expired grant and a mistyped mailbox are not the same problem: the first is fixed on the
// grants page and the second in the client. Anything the mail taxonomy recognises keeps the
// code the client was given, so the page and the client's own logs agree. What is left is a
// refusal this server made on the arguments alone, which is a client bug rather than a
// failure of any mailbox, and is marked as one.
func RefusedAs(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrRevoked):
		return "grant_revoked"
	case errors.Is(err, ErrExpired):
		return "grant_expired"
	case errors.Is(err, ErrNoScope):
		return "no_scope"
	}
	if code := mail.Code(err); code != "error" {
		return code
	}
	return OutcomeInvalid
}

// Observe reacts to what an outcome says about the mailbox itself.
//
// Only one outcome means anything durable: credentials that no longer work will not start
// working, so the mailbox is marked and the enforcement path already refuses it with a
// message telling the operator to re-link. Every piece of that pathway existed — providers
// return the error, Valid refuses on the status, the mailboxes page renders it — except this
// write, so a dead mailbox reported itself on every call and showed as healthy on the page.
//
// It lives beside Record because Record is the one place every tool outcome passes through.
// Marking it at each call site instead is what left the pathway half-built.
//
// Failures are not returned: the caller is already reporting the real error, and a mailbox
// that stays marked healthy for one more call is a smaller problem than an error about
// bookkeeping displacing the one the caller asked about.
func (g *Gate) Observe(ctx context.Context, gr *Grant, acct mail.AccountID, outcome string) {
	if g.status == nil || outcome != mail.CodeAuthExpired {
		return
	}
	if err := g.status.SetAccountStatus(ctx, gr.OwnerID, acct, mail.StatusNeedsReauth); err != nil {
		slog.Default().Warn("could not mark a mailbox as needing re-linking",
			"account", acct, "err", err)
	}
}
