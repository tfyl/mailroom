package grant

import "testing"

// A grant with nothing recorded is every grant approved before modes existed, and it has to
// keep working exactly as it did. That means behaving as the middle setting: not the loosest,
// which would hand an upgrade more autonomy than anybody agreed to, and not the strictest,
// which would start queueing mail that went out yesterday.
func TestAnUnrecordedModeIsTheDefault(t *testing.T) {
	var none Mode

	if got := none.Resolved(); got != ModeConfirm {
		t.Errorf("a grant with no mode should behave as %q, got %q", ModeConfirm, got)
	}
	if none.Holds() {
		t.Error("a grant with no mode must not start holding what it used to send")
	}
	if none.Enforced() {
		t.Error("the default is wording, not enforcement, and must not claim otherwise")
	}
	if none.Recorded() {
		t.Error("nobody chose this mode, and the UI has to be able to tell")
	}
	// Everything the pages read comes from these, so an unset mode must answer all of them
	// rather than rendering a grant with three blanks on its card.
	for name, got := range map[string]string{
		"title": none.Title(), "summary": none.Summary(), "brief": none.Brief(),
	} {
		if got == "" {
			t.Errorf("an unrecorded mode has no %s, so a page describing it would be blank", name)
		}
	}
}

// A value written by a newer build resolves the same way an absent one does. The alternative
// is a grant that reads as something this build cannot enforce, which would be enforcement
// decided by whichever version last wrote the row.
func TestAnUnknownModeIsTheDefault(t *testing.T) {
	if got := Mode("supervised").Resolved(); got != ModeConfirm {
		t.Errorf("an unrecognised mode should behave as %q, got %q", ModeConfirm, got)
	}
	if Mode("supervised").Valid() {
		t.Error("an unrecognised mode is not a mode a form may set")
	}
}

// Only one of the three refuses anything. The UI states this beside every option, so the type
// has to be the thing that decides it rather than a table in a template.
func TestOnlyHoldIsEnforced(t *testing.T) {
	for _, m := range AllModes {
		if want := m == ModeHold; m.Enforced() != want {
			t.Errorf("%s: enforced = %v, want %v", m, m.Enforced(), want)
		}
		if m.Holds() != m.Enforced() {
			t.Errorf("%s: the enforcement question and the holding question must be the same one", m)
		}
	}
}

// Loosening is what the confirmation page exists for, and getting the direction wrong would
// mean confirming the safe change and applying the dangerous one silently.
func TestLooseningIsWhatMovesTowardsActingAlone(t *testing.T) {
	cases := []struct {
		from, to Mode
		looser   bool
	}{
		{ModeHold, ModeConfirm, true},
		{ModeHold, ModeUnattended, true},
		{ModeConfirm, ModeUnattended, true},
		{ModeUnattended, ModeConfirm, false},
		{ModeConfirm, ModeHold, false},
		{ModeUnattended, ModeUnattended, false},
		// An old grant read as `confirm`, so setting it explicitly to `confirm` is not a
		// loosening — and setting it to `hold` is a tightening rather than a change from
		// nothing.
		{"", ModeConfirm, false},
		{"", ModeUnattended, true},
		{"", ModeHold, false},
	}
	for _, c := range cases {
		if got := Looser(c.from, c.to); got != c.looser {
			t.Errorf("Looser(%q, %q) = %v, want %v", c.from, c.to, got, c.looser)
		}
	}
}

func TestParseModeRefusesWhatTheFormDoesNotOffer(t *testing.T) {
	for _, bad := range []string{"", "high", "medium", "low", "strict", "held", "cautious"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) should have been refused", bad)
		}
	}
	// Trimmed and lowercased, because a radio value that arrives with a stray space is a
	// browser quirk rather than a drifted form.
	for _, good := range []string{"hold", " Hold ", "UNATTENDED"} {
		if _, err := ParseMode(good); err != nil {
			t.Errorf("ParseMode(%q): %v", good, err)
		}
	}
}
