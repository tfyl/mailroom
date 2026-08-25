package zoho

import (
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// Zoho passes the static half of the contract from the day it is written, before any
// credentials exist. That is the point of splitting the suite: the structural claims a
// provider makes about itself are checkable without a mailbox.
func TestZohoStaticConformance(t *testing.T) {
	conformance.Static(t, &Provider{})
}

// Zoho still claims less than Gmail, and the set has now moved three times. Drafts brought
// draft and discard, a Destroyer brought destructive, and a SettingsManager brought settings.
// None of them needed this list edited on its own account, because the set is derived from the
// interfaces the type satisfies — the list below is the assertion, not the source.
//
// Some methods behind those interfaces refuse. Zoho publishes no way to edit a stored draft or
// to send one, and it cannot be told to switch a vacation reply on without dates mailroom does
// not carry. The capabilities are claimed anyway, on the same footing as sending, which is
// claimed while refusing what it cannot do. The alternative is worse in both directions:
// dropping an interface would make the operations that do work unreachable through a mailbox
// that performs them, and the refusals are typed and name the operation rather than failing
// obscurely.
//
// destructive is claimed on different grounds, and the difference is worth stating: all three
// of its methods do the thing they are named for. Binning and restoring are the move mode of
// updatemessage, and permanent deletion is the delete Zoho documents with expunge=true, so
// nothing there rests on a refusal. What the claim changes for a caller is that trashing Zoho
// mail is no longer reachable only by applying the Trash folder as an exclusive label.
func TestZohoClaimsOnlyWhatItImplements(t *testing.T) {
	caps := (&Provider{}).Capabilities()

	for _, c := range []mail.Capability{
		mail.CapRead, mail.CapAttachments, mail.CapSend, mail.CapLabels,
		mail.CapDraft, mail.CapDiscard, mail.CapDestructive, mail.CapSettings,
	} {
		if !caps.Has(c) {
			t.Errorf("expected zoho to support %q", c)
		}
	}
	for _, c := range []mail.Capability{
		mail.CapFilters,
	} {
		if caps.Has(c) {
			t.Errorf("zoho claims %q but does not implement it yet", c)
		}
	}
}

// The two providers must genuinely differ, or the seam has not been tested by adding one.
func TestZohoDeclaresItsQuirks(t *testing.T) {
	quirks := (&Provider{}).Quirks()

	var exclusive, derived bool
	for _, q := range quirks {
		if q == mail.QuirkExclusiveLabel {
			exclusive = true
		}
		if q == mail.QuirkDerivedThreads {
			derived = true
		}
	}
	if !exclusive {
		t.Error("zoho folders are exclusive and must be declared, since applying one moves the message")
	}
	// This assertion is the reverse of what it was, and the reversal is the point.
	//
	// It used to fail the build if Zoho declared derived threading, on the grounds that Zoho
	// returns a thread id of its own. Running the suite against a live mailbox showed that it
	// does not: /messages/view and /messages/search answer with no threadId field, and
	// /messages/search rejects the parameter with EXTRA_PARAM_FOUND. Mailroom reaches a thread
	// by treating a message's own id as its thread id, which holds only for the message that
	// started one. The grouping is an inference and has to say so.
	if !derived {
		t.Error("zoho cannot report which thread a message is in, so mailroom infers it; " +
			"a caller that is not told will read a one-message thread as proof there were no replies")
	}
}

func TestZohoIDsCarryFolderAndMessage(t *testing.T) {
	p := &Provider{account: mail.Account{ID: "acct_1"}}

	id := p.scoped("100", "200")
	if id.Account != "acct_1" {
		t.Errorf("id must be stamped with the mailroom account, got %q", id.Account)
	}

	folder, message, err := splitNative(id.Native)
	if err != nil {
		t.Fatalf("an id this provider produced was not parseable by it: %v", err)
	}
	if folder != "100" || message != "200" {
		t.Errorf("round trip lost data: got folder=%q message=%q", folder, message)
	}

	// Zoho needs both parts to fetch content, so a bare message id must be rejected rather
	// than producing a request that quietly 404s.
	if _, _, err := splitNative("200"); err == nil {
		t.Error("a native id without a folder must be refused")
	}
}

func TestZohoLabelNamespacesDoNotCollide(t *testing.T) {
	// A folder and a label can share a numeric id in Zoho, so the merged model has to keep
	// them apart or a delete could remove the wrong thing.
	kind, native, err := splitLabelID("folder:42")
	if err != nil || kind != "folder" || native != "42" {
		t.Errorf("folder id did not round trip: %q %q %v", kind, native, err)
	}
	kind, native, err = splitLabelID("label:42")
	if err != nil || kind != "label" || native != "42" {
		t.Errorf("label id did not round trip: %q %q %v", kind, native, err)
	}
	if _, _, err := splitLabelID("42"); err == nil {
		t.Error("an unnamespaced id must be refused rather than guessed at")
	}
}
