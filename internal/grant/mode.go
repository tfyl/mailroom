package grant

import (
	"fmt"
	"strings"
)

// Mode is how much a client may do on its own initiative, recorded on the grant it holds.
//
// It answers a question capabilities cannot. A capability says whether an action is within
// this client's reach at all; a mode says what has to happen before it is taken. The same
// grant that lets a nightly job send its own digest unattended is the wrong shape for an
// agent improvising in somebody's inbox, and the difference between them is not which
// permissions they hold — it is identical — but whether a human is in the loop.
//
// Two of the three are steering and nothing else, and the type is deliberately blunt about
// which. Steering is the text of a tool's Description: a model reads it and decides, and a
// model that decides otherwise is not stopped by anything. ModeHold is the one that refuses,
// and Holds is the only method on this type that any enforcement path consults.
type Mode string

const (
	// ModeUnattended: the client completes its task without checking back. Steering says so
	// out loud, because an agent that stops to ask in an unattended context is asking a
	// cron job for permission and will sit there until it times out.
	ModeUnattended Mode = "unattended"

	// ModeConfirm: the client is told to put privileged actions to its human and wait for an
	// answer before performing them. Nothing on this server refuses anything on account of
	// it. It is the default because it is the right posture for an agent with a person
	// attached, and because a default that enforced would break every grant approved before
	// modes existed.
	ModeConfirm Mode = "confirm"

	// ModeHold: privileged actions are not performed when the client calls for them. They
	// are recorded, the tool says plainly that nothing happened, and the mailbox's owner
	// approves or discards each one in mailroom's own web interface. This is the only mode
	// with teeth, and the teeth are the point: mailroom cannot make a client ask a human,
	// but it can decline to act until a human has said so somewhere only the operator can
	// reach.
	ModeHold Mode = "hold"
)

// DefaultMode is what a grant with nothing recorded behaves as, which is every grant
// approved before this existed. It is the middle setting rather than the strictest because
// upgrading mailroom must not start holding mail that was going out yesterday, and not the
// loosest because an upgrade must not quietly hand anything more autonomy either.
const DefaultMode = ModeConfirm

// AllModes is the canonical order used wherever modes are displayed: most autonomous first,
// which is the order the consequences descend in.
var AllModes = []Mode{ModeUnattended, ModeConfirm, ModeHold}

// Resolved is the mode this value actually means.
//
// Anything unrecognised — the empty string a grant approved before modes existed carries,
// or a value written by a newer build — resolves to DefaultMode. Every method below goes
// through it, so a zero-valued Mode behaves as the default everywhere rather than at the
// call sites that remembered to check.
func (m Mode) Resolved() Mode {
	for _, known := range AllModes {
		if m == known {
			return m
		}
	}
	return DefaultMode
}

// Recorded reports whether this is a mode somebody chose, as opposed to the default a grant
// falls back to. Only the UI cares: it is the difference between showing an operator what
// their grant does and telling them what they picked.
func (m Mode) Recorded() bool { return m != "" && m == m.Resolved() }

// Holds reports whether privileged actions are held for approval rather than performed.
// This is the single enforcement question, asked in internal/mcp before anything irreversible
// reaches a provider.
func (m Mode) Holds() bool { return m.Resolved() == ModeHold }

// Autonomy orders the modes by how much a client may do without a human. Comparing two of
// these is what decides whether a change to a grant's mode is a loosening — which is the same
// shape as widening a grant's scope, and is treated the same way.
func (m Mode) Autonomy() int {
	switch m.Resolved() {
	case ModeHold:
		return 0
	case ModeConfirm:
		return 1
	default:
		return 2
	}
}

// Looser reports whether moving from one mode to another hands the client more initiative.
// Tightening needs no ceremony: it can only leave a token doing less than it was.
func Looser(from, to Mode) bool { return to.Autonomy() > from.Autonomy() }

// Title is the mode as a heading: what it does, in the fewest words that still say it.
func (m Mode) Title() string {
	switch m.Resolved() {
	case ModeUnattended:
		return "Acts on its own"
	case ModeHold:
		return "Holds privileged actions for you"
	default:
		return "Asks you before privileged actions"
	}
}

// Summary is one sentence an operator reads on a page. It is written to be honest about
// which half of the product is doing the work, because that distinction is the whole safety
// story: two of these describe what the client is told, and one describes what this server
// refuses to do.
func (m Mode) Summary() string {
	switch m.Resolved() {
	case ModeUnattended:
		return "The client is told to finish the job without checking back, sending included. " +
			"Nothing here holds it up."
	case ModeHold:
		return "Sending, deleting, filters and the vacation responder are not carried out when " +
			"the client asks for them. Each one waits here until you approve it."
	default:
		return "The client is told to put sending, deleting and mailbox settings to you first. " +
			"That is instruction rather than enforcement — nothing here stops it."
	}
}

// Brief is the clause the consent screen's running summary appends, in the same voice as the
// rest of that line: it completes "…and it ". It lives here with the other wording so that
// the sentence a script puts on the page is a sentence this server wrote — the script copies
// text the template rendered rather than carrying copy of its own.
func (m Mode) Brief() string {
	switch m.Resolved() {
	case ModeUnattended:
		return "will use them without checking back."
	case ModeHold:
		return "cannot use them without your approval here."
	default:
		return "is told to ask you first, which is wording rather than a rule."
	}
}

// Enforced reports whether this mode is backed by a refusal rather than by wording alone.
// The UI says so beside every mode, in those terms, so that nobody reads a steering setting
// as a control.
func (m Mode) Enforced() bool { return m.Holds() }

func (m Mode) Valid() bool { return m != "" && m == m.Resolved() }

// ParseMode reads a mode from a form. An unrecognised value is an error rather than becoming
// the default: a submission naming a mode this build does not have is a form that has drifted
// from the server, and silently landing on `confirm` would hide it.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.TrimSpace(strings.ToLower(s)))
	if !m.Valid() {
		return "", fmt.Errorf("unknown mode %q: want unattended, confirm or hold", s)
	}
	return m, nil
}
