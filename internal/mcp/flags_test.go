package mcp

import (
	"context"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// recordingLabels records which of the two label-manager operations mail_modify reaches for,
// and with what. The distinction is the whole subject of this file: one of them is portable
// and the other is Gmail's vocabulary.
type recordingLabels struct {
	stubLabels
	updates []mail.FlagUpdate
	added   [][]mail.LabelID
	removed [][]mail.LabelID
}

func (r *recordingLabels) SetFlags(_ context.Context, _ []mail.ScopedID, update mail.FlagUpdate) error {
	r.updates = append(r.updates, update)
	return nil
}

func (r *recordingLabels) ApplyLabels(ctx context.Context, ids []mail.ScopedID, add, remove []mail.LabelID) error {
	r.added = append(r.added, add)
	r.removed = append(r.removed, remove)
	return r.stubLabels.ApplyLabels(ctx, ids, add, remove)
}

// Read and starred are a flag update, not a label change.
//
// They used to be sent as labels called UNREAD and STARRED, which is Gmail's naming and
// nobody else's. Zoho and Microsoft refuse an id from that namespace as malformed; IMAP,
// which cannot express a label removal at all, returned success having done nothing — so
// "mark these read" reported as done on every IMAP mailbox and changed none of them. The
// providers all have a flag operation; nothing was calling it.
func TestModifyMarksReadThroughFlagsRatherThanLabels(t *testing.T) {
	work := &recordingLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	read := true
	res, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"}, Read: &read,
	})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if res.IsError {
		t.Fatalf("marking a message read should work: %v", payload(t, res))
	}

	if len(work.updates) != 1 {
		t.Fatalf("read state must be written as a flag update, got %d updates", len(work.updates))
	}
	if work.updates[0].Read == nil || !*work.updates[0].Read {
		t.Errorf("the update should ask for read, got %+v", work.updates[0])
	}
	if work.updates[0].Starred != nil {
		t.Error("nobody asked about the star; leaving it nil is what keeps it where it was")
	}

	for _, labels := range append(work.added, work.removed...) {
		for _, l := range labels {
			if l == "UNREAD" || l == "STARRED" {
				t.Errorf("%q is a Gmail label id and means nothing to the other three "+
					"providers; flags do not travel as labels", l)
			}
		}
	}
}

// Starring alone must say nothing about read state, for the same reason in the other
// direction: an absolute pair of flags cannot express one of them.
func TestModifyStarsWithoutTouchingReadState(t *testing.T) {
	work := &recordingLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	starred := true
	if _, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"}, Starred: &starred,
	}); err != nil {
		t.Fatalf("modify: %v", err)
	}

	if len(work.updates) != 1 {
		t.Fatalf("expected one flag update, got %d", len(work.updates))
	}
	if work.updates[0].Read != nil {
		t.Errorf("starring said something about read state: %+v", work.updates[0])
	}
}

// Archiving stays a label change, because that is what it is. It has to reach ApplyLabels so
// that a provider with folders can refuse it by name rather than be handed a flag it has no
// field for.
func TestModifyArchivesAsALabelChange(t *testing.T) {
	work := &recordingLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	if _, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"}, Archive: true,
	}); err != nil {
		t.Fatalf("modify: %v", err)
	}

	if len(work.removed) != 1 || len(work.removed[0]) != 1 || work.removed[0][0] != "INBOX" {
		t.Errorf("archiving is removing the inbox label, got %v", work.removed)
	}
	if len(work.updates) != 0 {
		t.Errorf("archiving is not a flag change: %v", work.updates)
	}
}

// A call that asks for nothing changes nothing, and must say so rather than report a count of
// messages it did not modify.
func TestModifyRefusesACallThatAsksForNoChange(t *testing.T) {
	work := &recordingLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	res, _, err := tools.handleModify(grantOver(mail.CapRead, mail.CapLabels), nil, modifyArgs{
		IDs: []string{"acct_1:m1"},
	})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a modify with nothing to modify reported success: %v", payload(t, res))
	}
	if len(work.updates) != 0 || len(work.added) != 0 {
		t.Error("nothing should have reached the provider")
	}
}
