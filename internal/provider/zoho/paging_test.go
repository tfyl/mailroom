package zoho

import (
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Zoho pages by offset into a list it does not order stably, so a walk can return the same
// message twice — and by the same token can step over one, which is the half that loses mail
// rather than merely repeating it.
//
// Measured on the live mailbox, deterministically: over an eight-page walk of ten, one
// message arrived at page 3 position 8 and again at page 4 position 0, in every run. It is
// not a race with delivery — the head of the list was identical before and after the walk, so
// nothing had arrived — and setting sortorder explicitly changed nothing.
//
// The arithmetic is right: start is 1-indexed and advances by exactly the page size. That is
// what makes this worth declaring rather than fixing. A stateless cursor cannot deduplicate
// across calls, so the honest thing is to tell callers, and this test is here so nobody
// quietly drops the warning while the behaviour is still there.
func TestZohoDeclaresThatItsPagingCanRepeat(t *testing.T) {
	p := &Provider{}

	var found bool
	for _, q := range p.Quirks() {
		if q == mmail.QuirkUnstablePaging {
			found = true
		}
	}
	if !found {
		t.Fatalf("Zoho must declare %q so a caller knows to deduplicate by id across pages; got %v",
			mmail.QuirkUnstablePaging, p.Quirks())
	}
}
