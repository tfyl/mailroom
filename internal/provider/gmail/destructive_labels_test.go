package gmail

import (
	"context"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Adding TRASH through BatchModify is the same act as Users.Messages.Trash, and the permission
// model can only tell them apart if this provider says so.
func TestGmailClassifiesItsOwnBin(t *testing.T) {
	p := &Provider{}

	for _, tc := range []struct {
		label mmail.LabelID
		want  mmail.LabelEffect
	}{
		{"TRASH", mmail.EffectTrash},
		{"SPAM", mmail.EffectSpam},
		{"INBOX", mmail.EffectFile},
		{"STARRED", mmail.EffectFile},
		{"UNREAD", mmail.EffectFile},
		// A label somebody made is Label_<n> whatever they called it, so the bin cannot be
		// impersonated and cannot be renamed out of recognition either.
		{"Label_17", mmail.EffectFile},
	} {
		got, err := p.EffectOfApplying(context.Background(), tc.label)
		if err != nil {
			t.Fatalf("classifying %q: %v", tc.label, err)
		}
		if got != tc.want {
			t.Errorf("applying %q classified as %q, want %q", tc.label, got, tc.want)
		}
		if got.Destructive() != (tc.want != mmail.EffectFile) {
			t.Errorf("applying %q reported destructive=%v", tc.label, got.Destructive())
		}
	}
}
