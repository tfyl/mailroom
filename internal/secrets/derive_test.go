package secrets

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// Built rather than written out. A 32-byte base64 literal in a source file is
// indistinguishable from a real key to anything reading the repository — including the
// secret scanner in CI, which is right to say so.
var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("mailroom-test-"), 3)[:KeyLen])

// The property the derivation exists for: the URL signing key is not the sealing key, and two
// purposes do not share one. Without both, compromising a signed link would be compromising
// every stored mailbox credential.
func TestDerivedKeysAreSeparateFromTheRootAndFromEachOther(t *testing.T) {
	root, err := decodeKey(testKey)
	if err != nil {
		t.Fatal(err)
	}

	signing, err := Derive(testKey, "mailroom attachment url signing v1", 32)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	other, err := Derive(testKey, "something else entirely", 32)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(signing, root) {
		t.Error("a derived key must not be the key it came from")
	}
	if bytes.Equal(signing, other) {
		t.Error("two purposes must not derive the same key")
	}
	if len(signing) != 32 {
		t.Errorf("want 32 bytes, got %d", len(signing))
	}
}

func TestDerivationIsStable(t *testing.T) {
	first, err := Derive(testKey, "purpose", 32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Derive(testKey, "purpose", 32)
	if err != nil {
		t.Fatal(err)
	}
	// Restarting the server must not invalidate every link it has handed out.
	if !bytes.Equal(first, second) {
		t.Error("the same key and purpose must derive the same bytes every time")
	}
}

func TestDerivationRefusesAnEmptyPurpose(t *testing.T) {
	if _, err := Derive(testKey, "", 32); err == nil {
		t.Fatal("an empty purpose is not separation from anything")
	}
	if _, err := Derive("", "purpose", 32); err == nil {
		t.Fatal("a missing key must fail rather than deriving from nothing")
	}
}
