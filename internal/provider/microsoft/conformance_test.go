package microsoft

import (
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// The static half of the contract needs no credentials, so a provider written against
// documentation can be held to it on the day it is written — which is the whole reason the
// suite is split, and the only half this provider can pass until somebody has a Microsoft
// mailbox to point Live at.
func TestMicrosoftStaticConformance(t *testing.T) {
	conformance.Static(t, &Provider{})
}

// Graph serves the whole capability surface, which is why this provider exists rather than
// an IMAP connector for the same mailboxes: rules and mailbox settings are the two that plain
// IMAP has nothing at all to address.
func TestMicrosoftClaimsEveryCapabilityItImplements(t *testing.T) {
	caps := (&Provider{}).Capabilities()

	for _, c := range mail.AllCapabilities {
		if !caps.Has(c) {
			t.Errorf("expected microsoft to support %q", c)
		}
	}
}

// The optional settings interfaces are deliberately absent. Graph v1.0 has no API for
// delegation, for the mailbox-level forwarding rule, or for whether IMAP is switched on, so
// implementing them would mean stubs that fail at call time — the shape the provider package
// exists to avoid.
func TestMicrosoftDoesNotImplementSettingsGraphHasNoAPIFor(t *testing.T) {
	var p any = &Provider{}

	if _, ok := p.(mail.DelegateManager); ok {
		t.Error("Graph v1.0 exposes no mailbox delegation; implementing it would be a stub")
	}
	if _, ok := p.(mail.ForwardingReader); ok {
		t.Error("Graph v1.0 exposes no mailbox-level forwarding rule")
	}
	if _, ok := p.(mail.IMAPSettingsReader); ok {
		t.Error("Graph v1.0 does not report whether IMAP is enabled on a mailbox")
	}
}

// Two providers that declare the same quirks have not tested the seam. Folders are exclusive
// as they are on Zoho and IMAP, batches are genuinely looped as they are on IMAP, and
// threading is authoritative as it is on Gmail and Zoho.
func TestMicrosoftDeclaresItsQuirks(t *testing.T) {
	quirks := (&Provider{}).Quirks()

	declared := map[mail.Quirk]bool{}
	for _, q := range quirks {
		declared[q] = true
	}

	if !declared[mail.QuirkExclusiveLabel] {
		t.Error("a Graph folder is exclusive and must be declared, since applying one moves the message")
	}
	if !declared[mail.QuirkNoBatch] {
		t.Error("a move and a property patch are addressed one message at a time; a caller sizing a batch needs to know")
	}
	if declared[mail.QuirkDerivedThreads] {
		t.Error("Exchange assigns conversationId; declaring derived threading would mislead callers")
	}
}

// The two namespaces have to survive a round trip, or applying a category could delete a
// folder that happens to share its name.
func TestMicrosoftLabelNamespacesDoNotCollide(t *testing.T) {
	kind, native, err := splitLabelID(folderLabel("AAMkAD=="))
	if err != nil || kind != labelFolder || native != "AAMkAD==" {
		t.Errorf("folder id did not round trip: %q %q %v", kind, native, err)
	}

	// A category is identified by its display name, which may contain anything a person can
	// type — a colon among it. Splitting on the first one is what keeps that intact.
	kind, native, err = splitLabelID(categoryLabel("Project: Apollo"))
	if err != nil || kind != labelCategory || native != "Project: Apollo" {
		t.Errorf("category name did not round trip: %q %q %v", kind, native, err)
	}

	if _, _, err := splitLabelID("AAMkAD=="); err == nil {
		t.Error("an unnamespaced id must be refused rather than guessed at")
	}
	if _, _, err := splitLabelID("tag:something"); err == nil {
		t.Error("an unknown namespace must be refused")
	}
}

// A message id is the whole native part here, unlike Zoho and IMAP where a folder has to
// travel with it. That is only true because immutable ids are requested everywhere; an
// ordinary Graph id encodes the folder and stops resolving when the message moves.
func TestMicrosoftIDsCarryOnlyTheMessage(t *testing.T) {
	p := &Provider{account: mail.Account{ID: "acct_1"}}

	id := p.scoped("AAMkAD==")
	if id.Account != "acct_1" {
		t.Errorf("id must be stamped with the mailroom account, got %q", id.Account)
	}
	if id.Native != "AAMkAD==" {
		t.Errorf("native id = %q", id.Native)
	}
}
