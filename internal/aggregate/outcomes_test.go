package aggregate

import (
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

var (
	work    = mail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"}
	archive = mail.Account{ID: "acct_2", Alias: "archive", Address: "archive@example.net"}
)

// One mailbox succeeding is enough for the call to have succeeded — that is the whole
// promise, and it is the property the write tools rely on to keep partial work.
func TestOneSuccessIsNotAFailure(t *testing.T) {
	o := NewOutcomes()
	o.OK(work, map[string]any{"modified": 3})
	o.Fail(archive, mail.ErrNeedsReauth)
	o.Reject("not-an-id", mail.ErrNotFound)

	if o.Failed() {
		t.Error("a call that modified three messages in one mailbox did not fail")
	}

	payload := o.Payload()
	accounts := payload["accounts"].(map[string]any)
	failure := accounts["archive"].(map[string]any)
	if failure["error"] != "auth_expired" {
		t.Errorf("a failure must carry the code a client acts on, got %v", failure["error"])
	}
	if len(payload["rejected"].([]map[string]any)) != 1 {
		t.Errorf("an id that reached no mailbox has to be reported, got %v", payload["rejected"])
	}
}

// An entry keyed "work" says nothing about which mailbox work is. Both halves of the block
// name the address, because a failure is exactly where somebody has to decide whether the
// mailbox that refused is the one they meant.
func TestEveryOutcomeNamesItsAddress(t *testing.T) {
	o := NewOutcomes()
	o.OK(work, map[string]any{"modified": 3})
	o.Fail(archive, mail.ErrNeedsReauth)

	accounts := o.Payload()["accounts"].(map[string]any)
	if got := accounts["work"].(map[string]any)["address"]; got != "work@example.com" {
		t.Errorf("a successful mailbox must name its address, got %v", got)
	}
	if got := accounts["archive"].(map[string]any)["address"]; got != "archive@example.net" {
		t.Errorf("a failing mailbox must name its address, got %v", got)
	}
	// The key stays the alias: it is what the caller named the mailbox by, and what it will
	// name it by again.
	if _, ok := accounts["work"]; !ok {
		t.Errorf("the block must stay keyed by alias, got %v", accounts)
	}
}

func TestNothingSucceedingIsAFailure(t *testing.T) {
	o := NewOutcomes()
	o.Fail(work, mail.ErrNeedsReauth)
	if !o.Failed() {
		t.Error("a call where every mailbox failed must not read as an empty success")
	}
	if _, reported := o.Payload()["rejected"]; reported {
		t.Error("a call with no rejected ids should not carry an empty rejected block")
	}
}
