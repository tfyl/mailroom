package imap

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Applying a label here is a MOVE into the named mailbox, which is the call Trash makes. So
// "move it to Trash" and "trash it" are one request with two names, and only this provider can
// say which of its mailbox names is which.
func TestIMAPClassifiesItsOwnBin(t *testing.T) {
	p := &Provider{}

	for _, tc := range []struct {
		mailbox mmail.LabelID
		want    mmail.LabelEffect
	}{
		{"Trash", mmail.EffectTrash},
		{"trash", mmail.EffectTrash},
		{"Junk", mmail.EffectSpam},
		{"Spam", mmail.EffectSpam},
		{"INBOX", mmail.EffectFile},
		{"Archive", mmail.EffectFile},
		{"Receipts", mmail.EffectFile},
		// The two delimiters servers actually use. "[Gmail]/Trash" is Gmail over IMAP and
		// "INBOX.Trash" is Courier and Dovecot's default; a classifier that only matched the
		// whole string would read both as ordinary folders.
		{"[Gmail]/Trash", mmail.EffectTrash},
		{"INBOX.Trash", mmail.EffectTrash},
	} {
		got, err := p.EffectOfApplying(context.Background(), tc.mailbox)
		if err != nil {
			t.Fatalf("classifying %q: %v", tc.mailbox, err)
		}
		if got != tc.want {
			t.Errorf("moving into %q classified as %q, want %q", tc.mailbox, got, tc.want)
		}
	}
}

// IMAP's own delete is \Deleted plus an expunge, and there is no route to it from the label
// path — asserted rather than assumed, because "the provider cannot express it" is a claim
// somebody will otherwise have to re-derive.
//
// FlagUpdate carries read and starred and nothing else, so the flags a modify can write are
// exactly \Seen and \Flagged. Every combination is checked, including the ones that ask for
// nothing, because a bug that leaked \Deleted into a STORE would most likely do it on the
// empty path.
func TestNoFlagUpdateCanReachDeleted(t *testing.T) {
	yes, no := true, false
	for _, update := range []mmail.FlagUpdate{
		{},
		{Read: &yes}, {Read: &no},
		{Starred: &yes}, {Starred: &no},
		{Read: &yes, Starred: &yes}, {Read: &yes, Starred: &no},
		{Read: &no, Starred: &yes}, {Read: &no, Starred: &no},
	} {
		add, remove := flagChanges(update)
		for _, flag := range append(append([]imap.Flag{}, add...), remove...) {
			if flag != imap.FlagSeen && flag != imap.FlagFlagged {
				t.Errorf("%+v wrote the flag %q, which is not one of the two a modify may set",
					update, flag)
			}
		}
	}
}
