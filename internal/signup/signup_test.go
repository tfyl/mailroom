package signup

import "testing"

// An unrecognised mode must fail rather than fall back. The whole point of the default is
// that it is restrictive, so a typo quietly resolving to anything else would defeat it.
func TestParseModeRejectsUnknown(t *testing.T) {
	if _, err := ParseMode("opeen"); err == nil {
		t.Fatal("a misspelled mode must be an error")
	}
	if m, err := ParseMode(""); err != nil || m != Closed {
		t.Fatalf("an unset mode must be closed, got %q %v", m, err)
	}
	if m, err := ParseMode("  OPEN "); err != nil || m != Open {
		t.Fatalf("modes are case- and space-insensitive, got %q %v", m, err)
	}
}

func TestAllowlistMatching(t *testing.T) {
	p := NewPolicy(Allowlist,
		[]string{"Ada@example.com", " "},
		[]string{"@team.example", "Other.Example"})

	for _, want := range []string{"ada@example.com", "ADA@Example.com", "someone@team.example", "x@other.example"} {
		if !p.AllowsEmail(want) {
			t.Errorf("%q should be allowed", want)
		}
	}
	for _, deny := range []string{"", "ada@other.com", "notada@example.com", "team.example", "x@sub.team.example"} {
		if p.AllowsEmail(deny) {
			t.Errorf("%q should not be allowed", deny)
		}
	}
}

// A subdomain is not the domain. Allowing team.example must not admit evil.team.example,
// which an unanchored suffix check would.
func TestAllowlistDomainIsExact(t *testing.T) {
	p := NewPolicy(Allowlist, nil, []string{"team.example"})
	if p.AllowsEmail("x@evil-team.example") || p.AllowsEmail("x@sub.team.example") {
		t.Fatal("domain matching must be exact")
	}
}

func TestCodeNormalisationSurvivesRetyping(t *testing.T) {
	code, err := NewCode()
	if err != nil {
		t.Fatal(err)
	}
	messy := "  " + code[:4] + " " + code[4:] + "\n"
	if HashCode(messy) != HashCode(code) {
		t.Fatal("a code retyped with spaces must still match")
	}
	if HashCode(code) == HashCode(code+"A") {
		t.Fatal("different codes must hash differently")
	}
}

func TestCodesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		c, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[c] {
			t.Fatal("NewCode repeated itself")
		}
		seen[c] = true
	}
}
