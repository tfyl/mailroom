package mail

import "testing"

func TestParseAlias(t *testing.T) {
	t.Run("accepts what the linking forms advertise", func(t *testing.T) {
		for _, in := range []string{"work", "Work-2", "personal_mail", "a", "1"} {
			got, err := ParseAlias(in)
			if err != nil {
				t.Errorf("ParseAlias(%q) = %v, want it accepted", in, err)
			}
			if got != in {
				t.Errorf("ParseAlias(%q) = %q, want it unchanged", in, got)
			}
		}
	})

	t.Run("trims surrounding space", func(t *testing.T) {
		got, err := ParseAlias("  work \n")
		if err != nil || got != "work" {
			t.Fatalf("ParseAlias = %q, %v; want \"work\", nil", got, err)
		}
	})

	// The HTML pattern attribute on the linking forms is advisory: a POST that never went
	// through a browser is exactly the case it does not cover.
	t.Run("refuses what only the browser was stopping", func(t *testing.T) {
		for _, in := range []string{
			"", "   ", "with space", "quote\"", "semi;colon", "slash/es",
			"at@sign", "new\nline", "emoji🙂", "tab\there",
		} {
			if _, err := ParseAlias(in); err == nil {
				t.Errorf("ParseAlias(%q) = nil, want an error", in)
			}
		}
	})

	t.Run("bounds the length", func(t *testing.T) {
		atLimit := make([]byte, MaxAliasLen)
		for i := range atLimit {
			atLimit[i] = 'a'
		}
		if _, err := ParseAlias(string(atLimit)); err != nil {
			t.Errorf("an alias of exactly %d should be accepted: %v", MaxAliasLen, err)
		}
		if _, err := ParseAlias(string(atLimit) + "a"); err == nil {
			t.Errorf("an alias of %d should be refused", MaxAliasLen+1)
		}
	})
}
