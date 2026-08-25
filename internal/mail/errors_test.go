package mail

import (
	"strings"
	"testing"
)

// A capability is not always unavailable as a whole. Gmail implements every settings
// operation and refuses exactly one of them on a consumer account, so reporting that as
// "does not support settings" tells a caller to stop trying five things that work.
func TestUnsupportedErrorNamesTheOperationWhenOnlyOneIsRefused(t *testing.T) {
	err := &UnsupportedError{
		Provider:   ProviderGmail,
		Account:    "work",
		Capability: CapSettings,
		Op:         "delegates",
		Reason:     "Gmail allows this only for a Workspace account",
	}

	msg := err.Error()
	if !strings.Contains(msg, `does not support "delegates"`) {
		t.Errorf("the message should name the operation, got: %s", msg)
	}
	if strings.Contains(msg, `"settings"`) {
		t.Errorf("naming the whole capability overstates what is unavailable, got: %s", msg)
	}
	if !strings.Contains(msg, "Workspace account") {
		t.Errorf("the reason should reach the caller, got: %s", msg)
	}
}

// Where the capability genuinely is absent, the message stays as it was: IMAP implements no
// filters at all, and naming an operation there would understate it.
func TestUnsupportedErrorNamesTheCapabilityWhenTheWholeThingIsMissing(t *testing.T) {
	err := &UnsupportedError{Provider: ProviderIMAP, Account: "archive", Capability: CapFilters}

	msg := err.Error()
	if !strings.Contains(msg, `does not support "filters"`) {
		t.Errorf("want the capability named, got: %s", msg)
	}
	if strings.HasSuffix(msg, ":") {
		t.Errorf("an absent reason should leave no dangling separator, got: %s", msg)
	}
}

// The classifier keys on the type, so callers matching on the code are unaffected by the
// message gaining detail.
func TestCodeIsUnchangedByTheAddedDetail(t *testing.T) {
	bare := &UnsupportedError{Provider: ProviderGmail, Account: "a", Capability: CapSettings}
	detailed := &UnsupportedError{Provider: ProviderGmail, Account: "a", Capability: CapSettings,
		Op: "delegates", Reason: "because"}

	if Code(bare) != Code(detailed) {
		t.Fatalf("the code must not depend on the detail: %q vs %q", Code(bare), Code(detailed))
	}
	if Code(bare) != "unsupported_by_provider" {
		t.Fatalf("unexpected code %q", Code(bare))
	}
}
