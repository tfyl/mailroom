package imap

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Marking mail read must not unstar it.
//
// The flag model used to be absolute: SetFlags took a whole Flags, so "mark this read" also
// said "and it is not starred", and IMAP's STORE would duly remove \Flagged. Nobody asked for
// that, and nothing in the result would have mentioned it.
func TestMarkingReadLeavesTheFlagAlone(t *testing.T) {
	p := newTestProvider(t, 1)
	ctx := context.Background()

	page, err := p.Search(ctx, mmail.Query{Limit: 1}, "")
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("nothing to flag: %v", err)
	}
	id := page.Items[0].ID

	if err := p.SetFlags(ctx, []mmail.ScopedID{id}, mmail.FlagUpdate{Starred: ptr(true)}); err != nil {
		t.Fatalf("starring: %v", err)
	}
	if err := p.SetFlags(ctx, []mmail.ScopedID{id}, mmail.FlagUpdate{Read: ptr(true)}); err != nil {
		t.Fatalf("marking read: %v", err)
	}

	got, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !got.Flags.Read {
		t.Error("the message should be read")
	}
	if !got.Flags.Starred {
		t.Error("marking a message read must not clear the flag on it: nobody asked for that")
	}
}

// An update that names nothing sends nothing. A STORE with an empty flag list is a request
// the server has no reason to receive.
func TestAnEmptyFlagUpdateTouchesNothing(t *testing.T) {
	add, remove := flagChanges(mmail.FlagUpdate{})
	if len(add) != 0 || len(remove) != 0 {
		t.Errorf("an empty update produced STORE work: add=%v remove=%v", add, remove)
	}

	add, remove = flagChanges(mmail.FlagUpdate{Read: ptr(false)})
	if len(add) != 0 || len(remove) != 1 || remove[0] != imap.FlagSeen {
		t.Errorf("marking unread should remove \\Seen and nothing else: add=%v remove=%v", add, remove)
	}
}

// A removal cannot be honoured here and must not be reported as though it had been.
//
// A message on IMAP is in exactly one mailbox and never in none of them, so there is nothing
// "remove this label" could do. It used to return nil: every archive, and every attempt to
// unlabel, came back successful having changed nothing at all. mail_modify translates its
// archive flag into removing INBOX, so archiving on an IMAP mailbox reported success on every
// call and moved no mail.
func TestRemovingALabelIsRefusedRatherThanIgnored(t *testing.T) {
	p := newTestProvider(t, 1)
	ctx := context.Background()

	page, err := p.Search(ctx, mmail.Query{Limit: 1}, "")
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("nothing to modify: %v", err)
	}
	ids := []mmail.ScopedID{page.Items[0].ID}

	err = p.ApplyLabels(ctx, ids, nil, []mmail.LabelID{"INBOX"})
	if err == nil {
		t.Fatal("removing a label does nothing here, so reporting success is a lie the caller " +
			"has no way to detect")
	}
	var unsupported *mmail.UnsupportedError
	if !asUnsupported(err, &unsupported) {
		t.Fatalf("want UnsupportedError so a caller can tell this from a failure worth "+
			"retrying, got %T: %v", err, err)
	}
	if unsupported.Op == "" {
		t.Error("the refusal must name the operation; moving a message works, and a caller " +
			"told that labels are unsupported here would stop trying")
	}
}

// Two exclusive labels at once is not a request that can be honoured either, and it has to
// refuse in the same vocabulary as everything else rather than as a bare error.
func TestMovingToTwoMailboxesAtOnceIsRefusedByName(t *testing.T) {
	p := newTestProvider(t, 1)

	err := p.ApplyLabels(context.Background(),
		[]mmail.ScopedID{{Account: "acct_imap", Native: "INBOX/1"}},
		[]mmail.LabelID{"Archive", "Projects"}, nil)

	var unsupported *mmail.UnsupportedError
	if !asUnsupported(err, &unsupported) {
		t.Fatalf("want UnsupportedError, got %T: %v", err, err)
	}
}

func ptr[T any](v T) *T { return &v }
