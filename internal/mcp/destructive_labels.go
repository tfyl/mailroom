package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// Trashing by another name.
//
// `destructive` gates mail_trash and `hold` queues it, and neither did anything about the
// same act performed through a label: adding TRASH on Gmail, moving into the Trash mailbox on
// IMAP, into a folder id on Zoho or Graph. mail_modify was gated on `labels`, which the
// consent screen offers as ordinary — so a grant nobody meant to trust with deletion could
// bin somebody's mail, and could do it under the one mode whose value is that it refuses.
//
// The rule is one sentence, and it is written here once rather than at each tool: a label
// operation whose effect is destruction needs `destructive` as well as `labels`, and is held
// exactly as mail_trash is.
//
// Three things enforce it, and the split is deliberate.
//
//   - The provider classifies. mail.LabelManager.EffectOfApplying is on the interface, so the
//     compiler asks every provider which of its own ids is the bin. Matching provider strings
//     up here instead would mean the next provider's bin is ordinary until somebody remembers
//     to add it, which is exactly how this hole appeared.
//   - The tool decides. Only the handler can turn a destructive change into a queued action
//     with a summary its owner can read, so the hold has to be arranged where the result is
//     built.
//   - The provider boundary refuses. labelManager hands the tools a guarded manager rather
//     than the provider's own, so a label change that reaches a provider without the handler
//     having asked is refused rather than performed. That is the part that survives the next
//     tool somebody writes.

// guardedLabels is the label manager the tools are given, in place of the provider's.
//
// It is a backstop, not the check: handleModify asks first, because only it can hold. What
// this catches is the call that did not ask — a new tool, or a path through an old one — and
// it fails closed on the assumption that anything reaching a provider unasked is a mistake.
type guardedLabels struct {
	mail.LabelManager
	acct mail.Account
}

func (l guardedLabels) ApplyLabels(ctx context.Context, ids []mail.ScopedID, add, remove []mail.LabelID) error {
	g := grantFrom(ctx)
	if g == nil {
		return errors.New("no grant on this request")
	}

	applies, err := mail.DestructiveApplies(ctx, l.LabelManager, add)
	if err != nil {
		return err
	}
	if len(applies) == 0 {
		return l.LabelManager.ApplyLabels(ctx, ids, add, remove)
	}
	if !g.Caps.Has(mail.CapDestructive) {
		return destructiveRefusal(g, l.acct, applies)
	}
	if g.Mode.Holds() {
		return fmt.Errorf(
			"nothing was done: applying %s to a message in %s %s, and this connection holds "+
				"destructive actions for the mailbox's owner to approve",
			mail.DescribeApplies(applies), l.acct.Alias, verbFor(applies))
	}
	return l.LabelManager.ApplyLabels(ctx, ids, add, remove)
}

// DeleteLabel is guarded for the same reason ApplyLabels is, and for the same kind of call:
// one that reaches a provider without the handler having asked. Promoting it from the embedded
// manager is what let a folder deletion through on nothing but `labels` — the guard existed
// and this method simply was not part of it.
func (l guardedLabels) DeleteLabel(ctx context.Context, id mail.LabelID) error {
	g := grantFrom(ctx)
	if g == nil {
		return errors.New("no grant on this request")
	}

	destroys, err := l.LabelManager.DeletingDestroysMail(ctx, id)
	if err != nil {
		return err
	}
	if !destroys {
		return l.LabelManager.DeleteLabel(ctx, id)
	}
	if !g.Caps.Has(mail.CapDestructive) {
		return deleteRefusal(g, l.acct, id)
	}
	if g.Mode.Holds() {
		return fmt.Errorf(
			"nothing was done: deleting %s in %s destroys the mail filed under it, and this "+
				"connection holds destructive actions for the mailbox's owner to approve",
			id, l.acct.Alias)
	}
	return l.LabelManager.DeleteLabel(ctx, id)
}

// destructiveRefusal is what a grant holding `labels` and not `destructive` is told when it
// asks for a label change that would destroy mail.// destructiveRefusal is what a grant holding `labels` and not `destructive` is told when it
// asks for a label change that would destroy mail.
//
// A ScopeError, so the client reads the same shape it reads for every other missing
// capability: the code it matches on, the capability to ask the operator for, and what the
// grant does hold. The sentence after it is the part that is particular to this refusal —
// without it, a client told that labelling needs `destructive` has no way to work out which
// of the labels it named was the problem, and its likeliest next move is to try them one at a
// time until something goes through.
func destructiveRefusal(g *grant.Grant, acct mail.Account, applies []mail.DestructiveApply) error {
	scope := &mail.ScopeError{
		Account: acct.Alias, Address: acct.Address,
		Capability: mail.CapDestructive, Held: g.Caps,
	}
	return fmt.Errorf("%w Applying %s is the same act as mail_trash, whatever it is called on "+
		"this provider, so it needs the same capability. Nothing in this call was performed.",
		scope, mail.DescribeApplies(applies))
}

// deleteRefusal is what a grant holding `labels` and not `destructive` is told when it asks
// to delete a label that is really a container.
//
// Worded around what is lost rather than around the capability, because the caller asked to
// delete a label and the surprise is that mail goes with it.
func deleteRefusal(g *grant.Grant, acct mail.Account, id mail.LabelID) error {
	scope := &mail.ScopeError{
		Account: acct.Alias, Address: acct.Address,
		Capability: mail.CapDestructive, Held: g.Caps,
	}
	return fmt.Errorf("%w Deleting %s on this provider deletes the mail filed under it, not "+
		"just the label, so it needs the same capability as mail_trash. Nothing was deleted.",
		scope, id)
}

// verbFor renders the effects as a clause for the sentence a held or refused call carries.// verbFor renders the effects as a clause for the sentence a held or refused call carries.
func verbFor(applies []mail.DestructiveApply) string {
	for _, a := range applies {
		if a.Effect == mail.EffectTrash {
			return "moves it to the bin"
		}
	}
	return "takes it out of the mailbox"
}

// destructiveSummary is the line the mailbox's owner reads on the held queue.
//
// It names the effect rather than only the label, because "apply TRASH to 4 messages in work"
// is a sentence somebody can approve without noticing what it does — which is the whole
// mistake this change is about.
func destructiveSummary(applies []mail.DestructiveApply, n int, alias string) string {
	return fmt.Sprintf("apply %s to %d %s in %s, which %s",
		mail.DescribeApplies(applies), n, plural(n, "message"), alias, verbFor(applies))
}
