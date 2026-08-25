package aggregate

import (
	"github.com/tfyl/mailroom/internal/mail"
)

// Outcomes collects what a fan-out did to each mailbox, for the tools that change mail
// rather than read it.
//
// Fan cannot serve those: there is nothing to merge and nothing to paginate. The property
// that matters is the same one, though, and it is the reason this lives here rather than
// being written again per tool — one failing mailbox must not lose the work already done in
// the others, and a failure has to arrive carrying the code that says whether retrying it is
// worth anything.
type Outcomes struct {
	accounts map[string]any
	rejected []map[string]any
	ok       int
}

func NewOutcomes() *Outcomes {
	return &Outcomes{accounts: map[string]any{}}
}

// OK records a mailbox the call succeeded against, with whatever detail describes what
// happened there — a count of messages modified, the id of a label deleted.
//
// Both halves take the whole account rather than its alias, and both write the address into
// the entry. The block is keyed by alias because that is the selector a caller hands back,
// so the address cannot replace the key; carried inside the entry it says which mailbox that
// alias currently is, which is the thing a caller reading "archive: 4 deleted" needs and
// could not previously get from the result at all.
//
// detail is a map rather than any so there is somewhere to put it. That is why listing
// labels reports {"labels": [...]} instead of a bare array: an array has no room for the
// name of the mailbox it came from.
func (o *Outcomes) OK(acct mail.Account, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["address"] = acct.Address
	o.accounts[acct.Alias] = detail
	o.ok++
}

// Fail records a mailbox the call failed against. The error travels as a code as well as a
// message, because a client that cannot tell a permanent refusal from a throttle retries the
// permanent one forever.
func (o *Outcomes) Fail(acct mail.Account, err error) {
	o.accounts[acct.Alias] = map[string]any{
		"address": acct.Address,
		"error":   mail.Code(err),
		"message": err.Error(),
	}
}

// Reject records an identifier that named no mailbox this call could act on: malformed, or
// naming one outside the grant. It is keyed by the id because there is no alias to key it
// by, and it is reported rather than dropped — a caller that asked for twenty messages and
// silently got nineteen will report to its user as though it changed all twenty.
func (o *Outcomes) Reject(id string, err error) {
	o.rejected = append(o.rejected, map[string]any{
		"id": id, "error": mail.Code(err), "message": err.Error(),
	})
}

// Failed reports whether nothing at all succeeded. Callers turn that into an error rather
// than an empty success, the same way an aggregated read does.
func (o *Outcomes) Failed() bool { return o.ok == 0 }

// Payload renders the response body: the per-account block every fan-out tool returns, plus
// the ids that reached no mailbox when there were any.
func (o *Outcomes) Payload() map[string]any {
	out := map[string]any{"accounts": o.accounts}
	if len(o.rejected) > 0 {
		out["rejected"] = o.rejected
	}
	return out
}
