package ids

import (
	"bytes"
	"sort"
	"strings"
	"testing"
	"time"
)

// The generator produces OAuth state values and pending-authorization ids as well as database
// keys, so a collision is not only a failed insert — it is one person's authorization
// answering to another person's request.
//
// An earlier version left 32 bits of entropy while looking like eighty, and collided after
// 67,196 draws. Two hundred thousand is comfortably past where that failed and nowhere near
// where eighty bits would.
func TestIdentifiersDoNotRepeat(t *testing.T) {
	seen := make(map[string]int, 200_000)
	for i := range 200_000 {
		id := New("test")
		if first, dup := seen[id]; dup {
			t.Fatalf("collision after %d ids: %q also drawn at %d", i, id, first)
		}
		seen[id] = i
	}
}

// Randomness must not overwrite the timestamp. When it did, identifiers minted seconds apart
// sorted arbitrarily, which quietly falsified the ordering the package promises and the audit
// log relies on.
func TestIdentifiersSortChronologically(t *testing.T) {
	var ids []string
	for range 5 {
		ids = append(ids, New("test"))
		time.Sleep(2 * time.Millisecond)
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids do not sort in generation order:\n generated %v\n sorted    %v", ids, sorted)
		}
	}
}

// Two identifiers drawn in the same millisecond share their timestamp and must differ
// entirely in the rest. A shared random half means randomness is landing where it should not.
//
// The comparison is made on the decoded bytes rather than the encoded characters, because
// base32 does not divide where the identifier does: six timestamp bytes are 48 bits, so the
// first nine characters are timestamp alone and the tenth straddles, carrying three timestamp
// bits beside two random ones. Counting shared leading characters therefore treats an eleventh
// agreeing character as a defect when it is two random bits and then five more agreeing by
// chance — about one draw in 128, which on a repository that runs the tests on every push is a
// red build nobody caused. Decoding puts the boundary where the generator put it, and ten
// random bytes agreeing is a 2^-80 event rather than a flake.
func TestIdentifiersDrawnTogetherShareOnlyTheirTimestamp(t *testing.T) {
	// Two draws land in the same millisecond nearly always, but not always, and a pair
	// straddling the turnover says nothing either way. Draw again rather than assert on it.
	const attempts = 100
	paired := false
	for range attempts {
		a, b := New("test"), New("test")
		stampA, randomA := splitIdentifier(t, a)
		stampB, randomB := splitIdentifier(t, b)
		if !bytes.Equal(stampA, stampB) {
			continue
		}
		if bytes.Equal(randomA, randomB) {
			t.Fatalf("identifiers drawn in the same millisecond share all %d random bytes: %q %q",
				randomBytes, a, b)
		}
		paired = true
		break
	}
	if !paired {
		// Reached when the timestamp half never repeats across a pair drawn back to back,
		// which is what randomness overwriting the timestamp looked like when it happened.
		t.Fatalf("no pair out of %d landed in the same millisecond", attempts)
	}

	// Differing is necessary and not sufficient. A counter would differ too, and the value
	// this half protects is an OAuth `state` that must be unguessable rather than merely
	// unique. So require every byte of it to vary across a batch: random bytes leave a
	// position constant with probability (1/256)^(draws-1), while a counter or a constant
	// freezes most positions on the first run.
	const draws = 64
	var first []byte
	varies := make([]bool, randomBytes)
	for i := range draws {
		_, random := splitIdentifier(t, New("test"))
		if i == 0 {
			first = random
			continue
		}
		for j := range randomBytes {
			if random[j] != first[j] {
				varies[j] = true
			}
		}
	}
	for j, ok := range varies {
		if !ok {
			t.Errorf("byte %d of the random half held one value across all %d draws", j, draws)
		}
	}
}

// splitIdentifier decodes an identifier back into the two halves New assembled it from.
func splitIdentifier(t *testing.T, id string) (stamp, random []byte) {
	t.Helper()

	body := strings.TrimPrefix(id, "test_")
	buf, err := encoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decoding %q: %v", id, err)
	}
	if len(buf) != timestampBytes+randomBytes {
		t.Fatalf("%q decodes to %d bytes, want %d", id, len(buf), timestampBytes+randomBytes)
	}
	return buf[:timestampBytes], buf[timestampBytes:]
}

func TestPrefixIsPreserved(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{Account(), "acct_"},
		{Grant(), "grant_"},
		{Client(), "client_"},
		{New("inv"), "inv_"},
	} {
		if !strings.HasPrefix(tc.got, tc.want) {
			t.Errorf("want prefix %q, got %q", tc.want, tc.got)
		}
	}
}

func TestTokensAreDistinctAndLong(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok, err := Token()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) < 50 {
			t.Fatalf("a bearer token should carry 256 bits, got %d characters", len(tok))
		}
		if seen[tok] {
			t.Fatal("Token repeated itself")
		}
		seen[tok] = true
	}
}
