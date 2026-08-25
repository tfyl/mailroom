package mail

import (
	"context"
	"strings"
)

// LabelEffect is what applying a label does to a message beyond filing it.
//
// It exists because "apply a label" and "destroy mail" are the same request on every provider
// mailroom speaks to. Gmail bins a message by adding TRASH; IMAP bins one by moving it into
// the mailbox called Trash, which is how this codebase applies an exclusive label; Zoho and
// Graph do the same with a folder id. A permission model that gates trashing and does not
// gate labelling gates nothing at all, and the label id alone cannot tell the two apart —
// only the provider knows which of its own ids is the bin.
//
// So the classification is part of the provider contract rather than a list of magic strings
// somewhere upstream. A new provider cannot be added without answering, for each of its
// labels, what applying it actually does.
type LabelEffect string

const (
	// EffectFile is an ordinary label or folder: the message is filed and nothing is lost.
	EffectFile LabelEffect = "file"

	// EffectTrash is the bin. Reversible in principle — the mail is still somewhere — and
	// destructive in practice, because every provider empties it on a timer nobody watches.
	EffectTrash LabelEffect = "trash"

	// EffectSpam is junk, which is the bin with an opinion attached. The mail leaves the
	// inbox, is purged on the same kind of timer, and the sender is taught to the filter, so
	// what it takes away includes mail that has not arrived yet.
	EffectSpam LabelEffect = "spam"
)

// Destructive reports whether applying a label with this effect destroys mail, and so needs
// CapDestructive rather than CapLabels alone.
//
// There is deliberately no permanent-delete effect. No provider here expresses an irreversible
// delete as a label: Gmail's batchDelete, Graph's permanentDelete and IMAP's \Deleted-then-
// expunge are all separate operations behind Destroyer, and IMAP's flag in particular is
// unreachable from the label path because FlagUpdate carries read and starred and nothing
// else. Adding an effect for it would describe a route that does not exist.
func (e LabelEffect) Destructive() bool {
	return e == EffectTrash || e == EffectSpam
}

// Describe says what the effect does, as a clause for a sentence a person reads: a refusal
// relayed by an agent, or the summary on a held action its owner is deciding about.
func (e LabelEffect) Describe() string {
	switch e {
	case EffectTrash:
		return "moves it to the bin"
	case EffectSpam:
		return "moves it to junk and teaches the filter to divert the sender"
	default:
		return "files it"
	}
}

// EffectOfMailboxName classifies a folder or label by the name a person sees on it.
//
// Shared by the providers whose label ids are, or resolve to, names. It is a name table and
// therefore an English one: a mailbox whose bin is called Papierkorb classifies as ordinary.
// That is a gap in coverage rather than in the design — the provider decides, and a provider
// with something better to go on than a name should use it, as Microsoft does with Graph's
// well-known folder ids.
func EffectOfMailboxName(name string) LabelEffect {
	// Hierarchies and Gmail's IMAP namespace: "[Gmail]/Trash" and "INBOX.Trash" are the bin
	// under two of the delimiters servers actually use.
	trimmed := strings.TrimSpace(name)
	if i := strings.LastIndexAny(trimmed, "/.\\"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	switch strings.ToLower(strings.TrimSpace(trimmed)) {
	case "trash", "bin", "deleted", "deleted items", "deleteditems", "deleted messages":
		return EffectTrash
	case "spam", "junk", "junk email", "junkemail", "junk e-mail", "bulk mail":
		return EffectSpam
	}
	return EffectFile
}

// DestructiveApply is one label in a proposed change whose effect destroys mail.
type DestructiveApply struct {
	Label  LabelID
	Effect LabelEffect
}

// DestructiveApplies classifies every label a change would apply and reports the ones that
// destroy mail.
//
// Only what is being applied. Removing a label never destroys anything on any provider here:
// removing INBOX is archiving, removing TRASH is restoring, and the folder providers cannot
// express a removal at all. Classifying removals would put a check on the one direction that
// only ever puts mail back.
//
// An error from the provider fails the whole classification rather than being treated as "no
// destructive labels here". A check that cannot be made has not passed.
func DestructiveApplies(ctx context.Context, m LabelManager, add []LabelID) ([]DestructiveApply, error) {
	var out []DestructiveApply
	for _, id := range add {
		effect, err := m.EffectOfApplying(ctx, id)
		if err != nil {
			return nil, err
		}
		if effect.Destructive() {
			out = append(out, DestructiveApply{Label: id, Effect: effect})
		}
	}
	return out, nil
}

// DescribeApplies renders a destructive change for a person: which labels, and what applying
// them does.
func DescribeApplies(applies []DestructiveApply) string {
	parts := make([]string, 0, len(applies))
	for _, a := range applies {
		parts = append(parts, string(a.Label)+" ("+a.Effect.Describe()+")")
	}
	return strings.Join(parts, ", ")
}
