package mail

import (
	"context"
	"io"
)

// Provider is the minimum every mail backend implements. Everything else is a narrow
// capability interface below.
//
// The alternative — one interface carrying every operation, with implementations stubbing
// the ones they cannot do — produces an interface shaped entirely by whichever provider was
// written first, and gives callers no way to distinguish "this failed" from "this was never
// possible here". Type assertions against small interfaces keep that distinction in the type
// system rather than in error strings.
type Provider interface {
	ID() ProviderID

	// Capabilities reports what this provider can do. It must agree with the interfaces the
	// implementation actually satisfies; the conformance suite checks that it does, because a
	// capability set that lies is worse than one that is merely narrow.
	Capabilities() Set

	// Quirks warns callers about behaviour that changes how results should be interpreted.
	Quirks() []Quirk
}

type MessageReader interface {
	Search(ctx context.Context, q Query, cursor string) (Page[Message], error)
	Get(ctx context.Context, id ScopedID) (Message, error)
}

type ThreadReader interface {
	GetThread(ctx context.Context, id ScopedID) (Thread, error)
}

type AttachmentReader interface {
	GetAttachment(ctx context.Context, msg ScopedID, attachmentID string) (Attachment, error)
}

type MessageWriter interface {
	Send(ctx context.Context, out Outgoing) (ScopedID, error)
}

type DraftManager interface {
	CreateDraft(ctx context.Context, out Outgoing) (ScopedID, error)
	UpdateDraft(ctx context.Context, id ScopedID, out Outgoing) error
	SendDraft(ctx context.Context, id ScopedID) (ScopedID, error)
	DeleteDraft(ctx context.Context, id ScopedID) error
	ListDrafts(ctx context.Context, cursor string) (Page[Message], error)
}

// FlagUpdate is a change to a message's flags, expressed as a delta: a nil field is left
// exactly as it was.
//
// A delta rather than a whole Flags, because "mark this read" is the request callers
// actually make and an absolute value cannot express it. Writing Flags{Read: true} says the
// message is read *and* unstarred, so a caller asking only about read state would silently
// lose the star — a change nobody asked for, to somebody's own filing, reported as success.
type FlagUpdate struct {
	Read    *bool
	Starred *bool
}

// Empty reports that this update asks for nothing, so a provider can skip the request
// rather than send one that changes nothing.
func (u FlagUpdate) Empty() bool { return u.Read == nil && u.Starred == nil }

// LabelManager covers labels, folders, and the read/star flags that ride alongside them.
//
// Flags are their own operation rather than labels named UNREAD and STARRED, because that
// naming is Gmail's alone. Gmail keeps both as labels; Zoho has a read integer and a
// follow-up flag; Exchange has isRead and flag/flagStatus; IMAP has \Seen and \Flagged.
// Sending a caller's "mark this read" through ApplyLabels hands three of the four providers
// a label id they have never heard of, which they either refuse confusingly or — on IMAP,
// where removals are not expressible at all — accept and ignore.
type LabelManager interface {
	ListLabels(ctx context.Context) ([]Label, error)
	CreateLabel(ctx context.Context, name string, exclusive bool) (Label, error)
	DeleteLabel(ctx context.Context, id LabelID) error
	ApplyLabels(ctx context.Context, ids []ScopedID, add, remove []LabelID) error
	SetFlags(ctx context.Context, ids []ScopedID, update FlagUpdate) error

	// EffectOfApplying reports what applying this label does to a message beyond filing it,
	// so that a label change which is really a trashing can be gated and held as one. See
	// LabelEffect in labels.go for why the provider is the one that has to answer.
	//
	// It is on this interface, rather than a table of provider strings kept upstream, so that
	// the compiler puts the question to every provider that can label at all. A provider that
	// answered EffectFile for its own bin would have made a decision somebody can find and
	// correct; one that was never asked leaves the same hole with nothing pointing at it.
	//
	// An id this provider does not recognise is EffectFile rather than an error: an id naming
	// nothing will fail in ApplyLabels, which is the call that can say so properly. An error
	// here is a lookup that could not be made — resolving a folder id over the network, and
	// failing — and it refuses the call rather than allowing it.
	EffectOfApplying(ctx context.Context, id LabelID) (LabelEffect, error)

	// DeletingDestroysMail reports whether deleting this label also destroys the mail filed
	// under it.
	//
	// A separate question from EffectOfApplying, because the answer is not related. Applying
	// a label is about where one message goes; deleting one is about whether a container and
	// its contents go with it. Gmail's labels are tags, so deleting one leaves every message
	// it was on — while on the three providers whose labels are folders, deleting is the most
	// destructive single call in the whole surface: it takes the folder and everything in it,
	// with no bin to recover from on IMAP.
	//
	// Answered by the provider for the same reason as EffectOfApplying: it is the only party
	// that knows whether a given id names a tag or a container, and putting the question on
	// the interface means a new provider cannot be added without answering it.
	//
	// An id this provider does not recognise is false, matching EffectOfApplying: an id
	// naming nothing will fail in DeleteLabel, which is the call that can say so properly.
	// An error here is a lookup that could not be made, and refuses the call.
	DeletingDestroysMail(ctx context.Context, id LabelID) (bool, error)
}

type Destroyer interface {
	Trash(ctx context.Context, ids []ScopedID) error
	Untrash(ctx context.Context, ids []ScopedID) error
	Delete(ctx context.Context, ids []ScopedID) error
}

// Filter is a server-side rule: criteria that match incoming mail, and actions applied to it.
//
// The actions are expressed as label changes rather than as named operations. "Archive" and
// "delete" are Gmail vocabulary for removing the inbox label and adding the trash one, and
// modelling them as their own booleans would bake one provider's naming into the contract —
// the same reason mail_modify takes labels rather than an archive flag.
type Filter struct {
	ID string

	// Criteria. Empty fields are not matched on.
	From          string
	To            string
	Subject       string
	Query         string
	NegatedQuery  string
	HasAttachment bool

	// Actions.
	AddLabels    []LabelID
	RemoveLabels []LabelID
	Forward      string
}

type FilterManager interface {
	ListFilters(ctx context.Context) ([]Filter, error)
	CreateFilter(ctx context.Context, f Filter) (Filter, error)
	DeleteFilter(ctx context.Context, id string) error
}

type SendAs struct {
	Address     string
	DisplayName string
	ReplyTo     string
	Default     bool
	Primary     bool
	Verified    bool
}

type Vacation struct {
	Enabled bool
	Subject string
	Body    string
	// RestrictToContacts and RestrictToDomain narrow who gets a reply. Worth carrying rather
	// than dropping: an auto-reply that goes to every stranger who mails you is a different
	// thing from one that answers colleagues.
	RestrictToContacts bool
	RestrictToDomain   bool
}

// Delegate is another address permitted to act on this mailbox. Verified reports whether the
// delegation has been accepted; an unverified delegate cannot yet read anything.
type Delegate struct {
	Address  string
	Verified bool
}

// Forwarding is the mailbox-level auto-forward rule, distinct from a per-filter forward.
type Forwarding struct {
	Enabled bool
	Address string
	// Disposition is what happens to the original: kept, archived, trashed, or marked read.
	Disposition string
}

type IMAPSettings struct {
	Enabled       bool
	AutoExpunge   bool
	MaxFolderSize int64
}

// SettingsManager is the capability-bearing interface for mailbox settings: the two things
// any provider with settings at all can be expected to do.
//
// Everything rarer lives in its own interface below, so a provider that has aliases but no
// delegation implements what it supports and no more. Bundling them would force a provider
// into stubs that fail at call time, which is the shape this package exists to avoid.
type SettingsManager interface {
	ListSendAs(ctx context.Context) ([]SendAs, error)
	GetVacation(ctx context.Context) (Vacation, error)
	SetVacation(ctx context.Context, v Vacation) error
}

// DelegateManager is implemented where a mailbox can be delegated to another address. On
// Gmail this is Workspace-only and needs a scope beyond the ordinary settings one.
type DelegateManager interface {
	ListDelegates(ctx context.Context) ([]Delegate, error)
}

// ForwardingReader exposes the mailbox-level auto-forward rule. Read-only on purpose:
// redirecting somebody's mail elsewhere is not something an agent should be able to arrange.
type ForwardingReader interface {
	GetForwarding(ctx context.Context) (Forwarding, error)
}

// IMAPSettingsReader exposes whether IMAP access is enabled on the mailbox.
type IMAPSettingsReader interface {
	GetIMAPSettings(ctx context.Context) (IMAPSettings, error)
}

// Streamer is implemented by attachment readers that can avoid buffering the whole payload.
type Streamer interface {
	StreamAttachment(ctx context.Context, msg ScopedID, attachmentID string) (io.ReadCloser, error)
}

// Supports reports whether p implements the interfaces backing c. It is the single place
// that maps a capability to the interfaces it needs, so a new capability cannot be added
// without deciding what implementing it means.
func Supports(p Provider, c Capability) bool {
	switch c {
	case CapRead:
		_, ok := p.(MessageReader)
		return ok
	case CapAttachments:
		_, ok := p.(AttachmentReader)
		return ok
	// Drafting and discarding share an interface because DeleteDraft lives on it: a provider
	// that can save a draft can always remove one. The two capabilities differ in what a
	// grant is trusted to do, not in what a provider can do, so both map here.
	case CapDraft, CapDiscard:
		_, ok := p.(DraftManager)
		return ok
	case CapSend:
		_, ok := p.(MessageWriter)
		return ok
	case CapLabels:
		_, ok := p.(LabelManager)
		return ok
	case CapFilters:
		_, ok := p.(FilterManager)
		return ok
	case CapSettings:
		_, ok := p.(SettingsManager)
		return ok
	case CapDestructive:
		_, ok := p.(Destroyer)
		return ok
	default:
		return false
	}
}

// DerivedCapabilities computes the capability set a provider genuinely satisfies, from the
// interfaces it implements. Implementations should return this from Capabilities rather than
// hand-maintaining a list that can drift.
func DerivedCapabilities(p Provider) Set {
	out := NewSet()
	for _, c := range AllCapabilities {
		if Supports(p, c) {
			out.Add(c)
		}
	}
	return out
}
