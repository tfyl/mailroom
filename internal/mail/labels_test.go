package mail

import (
	"context"
	"errors"
	"testing"
)

func TestEffectOfMailboxNameRecognisesTheBinAndJunk(t *testing.T) {
	for name, want := range map[string]LabelEffect{
		"Trash":           EffectTrash,
		"TRASH":           EffectTrash,
		" trash ":         EffectTrash,
		"Bin":             EffectTrash,
		"Deleted Items":   EffectTrash,
		"deleteditems":    EffectTrash,
		"[Gmail]/Trash":   EffectTrash,
		"INBOX.Trash":     EffectTrash,
		"Spam":            EffectSpam,
		"Junk":            EffectSpam,
		"Junk Email":      EffectSpam,
		"Inbox":           EffectFile,
		"Archive":         EffectFile,
		"Receipts":        EffectFile,
		"Trashed drafts":  EffectFile,
		"Not quite trash": EffectFile,
		"INBOX/Contracts": EffectFile,
		"":                EffectFile,
	} {
		if got := EffectOfMailboxName(name); got != want {
			t.Errorf("%q classified as %q, want %q", name, got, want)
		}
	}
}

// Only trash and junk destroy. Filing does not, and the distinction is what lets `labels`
// remain an ordinary capability rather than becoming a privileged one.
func TestOnlyTrashAndSpamAreDestructive(t *testing.T) {
	for effect, want := range map[LabelEffect]bool{
		EffectFile: false, EffectTrash: true, EffectSpam: true,
	} {
		if got := effect.Destructive(); got != want {
			t.Errorf("%q reported destructive=%v", effect, got)
		}
	}
}

// classifier is a label manager that answers from a table, so the collection logic can be
// tested without a provider behind it.
type classifier struct {
	LabelManager
	effects map[LabelID]LabelEffect
	err     error
}

func (c classifier) DeletingDestroysMail(_ context.Context, _ LabelID) (bool, error) {
	return false, nil
}

func (c classifier) EffectOfApplying(_ context.Context, id LabelID) (LabelEffect, error) {
	if c.err != nil {
		return "", c.err
	}
	return c.effects[id], nil
}

func TestDestructiveAppliesReportsEveryDestructiveLabel(t *testing.T) {
	c := classifier{effects: map[LabelID]LabelEffect{
		"TRASH": EffectTrash, "SPAM": EffectSpam, "Receipts": EffectFile,
	}}

	applies, err := DestructiveApplies(context.Background(), c,
		[]LabelID{"Receipts", "TRASH", "SPAM"})
	if err != nil {
		t.Fatalf("classifying: %v", err)
	}
	if len(applies) != 2 {
		t.Fatalf("expected both destructive labels, got %+v", applies)
	}
	if applies[0].Label != "TRASH" || applies[0].Effect != EffectTrash {
		t.Errorf("first destructive label is %+v", applies[0])
	}

	// Nothing applied is nothing to classify, which is the case every archive and every
	// mark-read takes.
	applies, err = DestructiveApplies(context.Background(), c, nil)
	if err != nil || len(applies) != 0 {
		t.Errorf("an empty change classified as %+v (%v)", applies, err)
	}
}

// A classification that could not be made has not passed. The alternative — treating a
// provider error as "nothing destructive here" — turns a throttle into a way past the
// capability.
func TestDestructiveAppliesFailsClosed(t *testing.T) {
	boom := errors.New("graph is unreachable")
	c := classifier{err: boom}

	if _, err := DestructiveApplies(context.Background(), c, []LabelID{"TRASH"}); !errors.Is(err, boom) {
		t.Fatalf("a failed classification answered %v", err)
	}
}
