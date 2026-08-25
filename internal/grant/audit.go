package grant

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Audit is one recorded call: who made it, against what, and how it ended.
//
// The first five fields were the whole of it, and between them they say "this grant called
// mail.modify against this mailbox and it was ok" — which is not the question anybody opens
// the audit page to ask. They open it because something has gone wrong, and the questions
// then are what was affected, why it was turned away, and how much of it there was. The rest
// of this struct is those questions.
//
// The shape is deliberate, and it is as much the security boundary as it is the schema. Four
// facts are columns because they mean the same thing for every tool and are what anybody
// would filter or aggregate on. Everything one tool knows and its neighbours do not is one
// bounded Detail value, because a column per tool does not survive the next tool. Detail is a
// struct rather than a map for the reason that matters most here: a message body cannot reach
// the audit log, because there is no field on it that would hold one.
type Audit struct {
	OwnerID   user.ID
	GrantID   ID
	AccountID mail.AccountID
	Tool      string
	// Outcome is "ok", the mail error code for a failure the gate or a provider produced, or
	// OutcomeInvalid for a call this server refused on its own arguments. Those three are
	// what let a reader tell a refusal from a broken mailbox from a broken client.
	Outcome string

	// Capability is the permission this call spent. Empty where a call needs none — discovery
	// and minting an upload URL — which is a fact about the call rather than a gap, and the
	// page says so in those words rather than leaving the space blank.
	Capability mail.Capability
	// Reason is the sentence behind a failure, and is the same text the client was given. The
	// outcome says which kind of failure it was; this says which failure, which is the whole
	// distance between "scope_denied" and "this grant holds read on work. That action
	// requires send".
	Reason string
	// Affected is the size of the set this call acted on — messages modified, results
	// returned, recipients written to. Nil rather than zero where a tool acts on no countable
	// set, so that "none" and "does not apply" stay different answers.
	Affected *int

	Detail Detail
	At     time.Time
}

// OutcomeInvalid marks a call this server refused on the arguments it arrived with, before
// any mailbox was consulted. It is not one of the mail error codes because it is not a
// failure of the mail: a client sending a modify that asks for no change, or a send with no
// recipient, is a client with a bug, and a page that showed that as `error` beside a provider
// outage would be inviting the operator to debug the wrong system.
const OutcomeInvalid = "invalid"

// Detail carries what one tool knows and the others do not.
//
// Every field here is metadata about what was touched or about what left, and the line they
// draw together is the one written down in docs/security.md: the log may name what a call
// affected and what it sent out of a mailbox, and may never hold what was in one. That is why
// there is a Subject — the subject of a message this server composed and sent — and no field
// for a body, a snippet, or the subject of a message that was merely read. A log that
// accumulated the second would become a copy of the mailbox it exists to be a control over.
type Detail struct {
	// Action is what the call did inside a tool that does several things: trash or delete,
	// create or update, which section of the settings was read.
	Action string `json:"action,omitempty"`
	// IDs names the messages, threads or drafts acted on, as scoped ids — the account and the
	// provider's own identifier, which is the pair that still resolves after the mail itself
	// has changed. Bounded, with More carrying however many were dropped.
	IDs  []string `json:"ids,omitempty"`
	More int      `json:"more,omitempty"`
	// Name is the label, filter or settings section a call named, where the thing it named
	// was not a message.
	Name string `json:"name,omitempty"`

	// To, Cc and Bcc are where this call directed mail: the recipients of a send or a draft,
	// and the destination of a forwarding rule, which routes mail out of a mailbox as surely
	// as a send does. Bounded the same way IDs is.
	To  []string `json:"to,omitempty"`
	Cc  []string `json:"cc,omitempty"`
	Bcc []string `json:"bcc,omitempty"`
	// Subject is the subject of a message this server sent, drafted, or set as an auto-reply.
	// Never the subject of one it read.
	Subject string `json:"subject,omitempty"`
}

func (d Detail) Empty() bool {
	return d.Action == "" && len(d.IDs) == 0 && d.More == 0 && d.Name == "" &&
		len(d.To) == 0 && len(d.Cc) == 0 && len(d.Bcc) == 0 && d.Subject == ""
}

// What one row is allowed to grow to.
//
// The interesting number is not what an ordinary row costs but what an unbounded one would: a
// modify across two thousand ids, recorded whole, is a row two thousand ids wide, written to
// the same volume as the database by a client that can call it in a loop. Ten ids are enough
// to recognise what a call was about, and the count beside them is the honest answer to how
// many there really were.
const (
	maxDetailItems = 10
	maxDetailText  = 200
	maxReason      = 300
)

// Bounded returns the entry as it should be stored. It is applied at the write itself rather
// than at each of the call sites that build one, so a new call site cannot forget it.
func (a Audit) Bounded() Audit {
	a.Reason = clip(a.Reason, maxReason)
	a.Detail.Subject = clip(a.Detail.Subject, maxDetailText)
	a.Detail.Action = clip(a.Detail.Action, maxDetailText)
	a.Detail.Name = clip(a.Detail.Name, maxDetailText)

	dropped := 0
	for _, list := range []*[]string{&a.Detail.IDs, &a.Detail.To, &a.Detail.Cc, &a.Detail.Bcc} {
		if len(*list) > maxDetailItems {
			dropped += len(*list) - maxDetailItems
			trimmed := make([]string, maxDetailItems)
			copy(trimmed, (*list)[:maxDetailItems])
			*list = trimmed
		}
		for i, v := range *list {
			(*list)[i] = clip(v, maxDetailText)
		}
	}
	a.Detail.More += dropped
	return a
}

// clip truncates on a rune boundary, so a cut landing inside a multi-byte character cannot
// leave a column holding invalid UTF-8.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " ") + "…"
}
