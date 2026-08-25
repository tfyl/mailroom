package mail

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is a unit of permission. Capabilities split where trust changes rather than
// where the provider API does: draft and send are separate because one is reversible and
// the other is not, and attachments are separate from read because fetching a contract is
// a different risk from reading a message body.
type Capability string

const (
	CapRead        Capability = "read"
	CapAttachments Capability = "attachments"
	CapDraft       Capability = "draft"
	CapDiscard     Capability = "discard"
	CapSend        Capability = "send"
	CapLabels      Capability = "labels"
	CapFilters     Capability = "filters"
	CapSettings    Capability = "settings"
	CapDestructive Capability = "destructive"
)

// AllCapabilities is the canonical order used wherever capabilities are displayed, roughly
// ascending in how much damage they permit.
//
// CapDiscard sits between drafting and sending because that is where it belongs on both
// counts. It is the same subject as CapDraft — unsent text in a mailbox the grant already
// composes into — and it is the first entry in this list that destroys anything, which is
// exactly why it is no longer part of CapDraft. Composing and destroying are different
// decisions, and a grant that may write a draft should not thereby be able to remove one a
// person wrote. Deleting a message that arrived stays CapDestructive: a draft is this
// grant's own work product, received mail is not.
var AllCapabilities = []Capability{
	CapRead, CapAttachments, CapDraft, CapDiscard, CapSend,
	CapLabels, CapFilters, CapSettings, CapDestructive,
}

// Privileged capabilities are visually flagged on the consent screen. They either send mail
// on the operator's behalf, destroy it, or extract file contents.
//
// CapDiscard is deliberately not among them, which is the one entry in this map worth
// arguing. Everything here reaches past the grant's own work: mail that arrived, file
// contents leaving the mailbox, the operator's name on an outgoing message, the mailbox's
// own behaviour. Discarding a draft reaches none of that, and it is the routine housekeeping
// of any agent that composes. Flagging it would put a warning-tinted box on the ordinary
// compose grant, and a warning that is ticked on every ordinary grant stops being read —
// which would cost the four that need reading. The split itself is what this change is for;
// the flag would not add to it.
var privileged = map[Capability]bool{
	CapSend:        true,
	CapDestructive: true,
	CapAttachments: true,
	CapSettings:    true,
}

func (c Capability) Privileged() bool { return privileged[c] }

func (c Capability) Valid() bool {
	for _, known := range AllCapabilities {
		if c == known {
			return true
		}
	}
	return false
}

func ParseCapability(s string) (Capability, error) {
	c := Capability(strings.TrimSpace(strings.ToLower(s)))
	if !c.Valid() {
		return "", fmt.Errorf("unknown capability %q", s)
	}
	return c, nil
}

// Set is an unordered collection of capabilities. The zero value is a valid empty set, which
// matters: a grant with no capabilities must behave as "denies everything" rather than
// panicking or, far worse, defaulting open.
type Set map[Capability]struct{}

func NewSet(caps ...Capability) Set {
	s := make(Set, len(caps))
	for _, c := range caps {
		s[c] = struct{}{}
	}
	return s
}

func (s Set) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

func (s Set) Add(c Capability) { s[c] = struct{}{} }

func (s Set) Len() int { return len(s) }

// Slice returns the set in AllCapabilities order so that output is stable across calls.
func (s Set) Slice() []Capability {
	out := make([]Capability, 0, len(s))
	for _, c := range AllCapabilities {
		if s.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

func (s Set) Strings() []string {
	caps := s.Slice()
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

func (s Set) String() string { return strings.Join(s.Strings(), ",") }

// Intersect returns the capabilities present in both sets.
func (s Set) Intersect(other Set) Set {
	out := NewSet()
	for c := range s {
		if other.Has(c) {
			out.Add(c)
		}
	}
	return out
}

// ParseSet reads a comma-separated capability list. Unknown entries are an error rather than
// being skipped: silently dropping an unrecognised capability from a stored grant would
// widen or narrow it without anybody noticing.
func ParseSet(s string) (Set, error) {
	out := NewSet()
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		c, err := ParseCapability(part)
		if err != nil {
			return nil, err
		}
		out.Add(c)
	}
	return out, nil
}

// SetFromStrings builds a set from arbitrary input, reporting any entries it did not
// recognise rather than discarding them.
func SetFromStrings(in []string) (Set, error) {
	out := NewSet()
	var unknown []string
	for _, s := range in {
		c, err := ParseCapability(s)
		if err != nil {
			unknown = append(unknown, s)
			continue
		}
		out.Add(c)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown capabilities: %s", strings.Join(unknown, ", "))
	}
	return out, nil
}
