package mail

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/user"
)

type (
	ProviderID string
	AccountID  string
	LabelID    string
)

const (
	ProviderGmail     ProviderID = "gmail"
	ProviderZoho      ProviderID = "zoho"
	ProviderIMAP      ProviderID = "imap"
	ProviderMicrosoft ProviderID = "microsoft"
)

type AccountStatus string

const (
	StatusLinked      AccountStatus = "linked"
	StatusNeedsReauth AccountStatus = "needs_reauth"
	StatusDisabled    AccountStatus = "disabled"
)

// Account is a linked mailbox. Alias is a mutable label; ID is what grants store, so that
// renaming a mailbox never changes who can reach it.
type Account struct {
	ID AccountID
	// OwnerID is the user this mailbox belongs to. It travels with the account so that
	// anything holding one — the provider factory, most of all — can scope its own lookups
	// without the owner having to be threaded separately alongside it.
	OwnerID    user.ID
	Alias      string
	Address    string
	Provider   ProviderID
	Status     AccountStatus
	LinkedAt   time.Time
	LastUsedAt time.Time
}

// Display names a mailbox the way it should be named back to whoever asked: the alias it is
// selected by, then the address it actually is.
//
// An alias is a label somebody chose, and two people's "main" are different mailboxes. An
// agent told only "main" cannot tell which one it just sent from, and neither can whoever
// reads the transcript afterwards — which is the moment it matters, because by then the mail
// has gone. This is for prose a person reads; structured results carry the two apart, so
// that the alias stays something a caller can hand straight back as a selector.
func (a Account) Display() string { return displayMailbox(a.Alias, a.Address) }

// displayMailbox is shared with the error types, which carry the same two values flattened
// rather than a whole Account. Either half may be missing — an error can name a mailbox that
// was never resolved — and naming one of them beats naming neither.
func displayMailbox(alias, address string) string {
	switch {
	case address == "":
		return alias
	case alias == "":
		return address
	default:
		return alias + " - " + address
	}
}

// Quirk warns a caller about provider behaviour that changes how results should be read.
type Quirk string

const (
	QuirkDerivedThreads Quirk = "derived_threads"
	QuirkExclusiveLabel Quirk = "exclusive_labels"
	QuirkNoBatch        Quirk = "no_batch"
	QuirkPartialSearch  Quirk = "partial_search"
	// QuirkUnstablePaging warns that walking a mailbox page by page can return the same
	// message twice, and by the same token can step over one. It is declared by a provider
	// whose cursor is an offset into a list it does not order stably, where the arithmetic
	// being right is not enough to make the walk right.
	//
	// A caller that must see every message exactly once has to deduplicate by id across
	// pages rather than trust the pagination to do it.
	QuirkUnstablePaging Quirk = "unstable_paging"
)

// ScopedID pairs an account with a provider-native identifier. It is rendered as
// "<account-id>:<provider-id>" and is deliberately built from the immutable account ID:
// aliases can be renamed at any time, and an ID a client is holding must not stop resolving
// because the operator retitled a mailbox.
type ScopedID struct {
	Account AccountID
	Native  string
}

func (s ScopedID) String() string { return string(s.Account) + ":" + s.Native }

func (s ScopedID) Zero() bool { return s.Account == "" && s.Native == "" }

func ParseScopedID(s string) (ScopedID, error) {
	account, native, found := strings.Cut(s, ":")
	if !found || account == "" || native == "" {
		return ScopedID{}, fmt.Errorf("malformed id %q: want <account>:<provider-id>", s)
	}
	return ScopedID{Account: AccountID(account), Native: native}, nil
}

type Address struct {
	Name  string
	Email string
}

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return (&mail.Address{Name: a.Name, Address: a.Email}).String()
}

type Body struct {
	Text string
	HTML string
}

type Flags struct {
	Read    bool
	Starred bool
	Draft   bool
}

// AttachmentRef describes an attachment without carrying its bytes. Listing attachments is
// covered by CapRead; fetching the content needs CapAttachments, so the manifest and the
// payload are deliberately separate types.
type AttachmentRef struct {
	ID       string
	Filename string
	MimeType string
	Size     int64
	Inline   bool
}

type Attachment struct {
	AttachmentRef
	Content []byte
}

// MaxAttachmentBytes is the most raw attachment content one outgoing message may carry.
//
// Gmail's limit is 25 MB on the assembled message, and base64 inflates by 4/3 plus line
// breaks, so this encodes to roughly 24.6 MB. It lives in this package rather than beside
// either of the two paths that enforce it — the compose tools, which sum it across a message,
// and the blob store, which caps a single upload — because two numbers that have to agree and
// are declared in two packages eventually will not. An upload larger than a whole message
// could hold would be accepted and stored, then refused at send, having already spent the
// disk.
const MaxAttachmentBytes = 18 << 20

type Message struct {
	ID ScopedID
	// Account is the mailbox's alias, for display; never parse this — the identifier is the
	// account ID inside ID. It stays the bare alias rather than becoming alias-and-address,
	// because it is also what the per-account status block of an aggregated read is keyed by,
	// and a row that no longer matches its own status entry is worse than a terse one. The
	// address is rendered beside it, as account_address.
	Account     string
	ThreadID    ScopedID
	From        Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	Subject     string
	Date        time.Time
	Snippet     string
	Body        Body
	Labels      []LabelID
	Flags       Flags
	Attachments []AttachmentRef
}

type Thread struct {
	ID       ScopedID
	Account  string
	Subject  string
	Messages []Message
	// Derived reports that this grouping was inferred rather than returned as a thread by the
	// provider — from References and In-Reply-To headers on IMAP, and on Zoho from a thread id
	// mailroom guessed at because no listing reports one. An agent asked to "reply to the last
	// message in this thread" should know whether the grouping was authoritative, and must not
	// read a short derived thread as proof that nobody replied.
	Derived bool
}

type LabelKind string

const (
	LabelSystem LabelKind = "system"
	LabelUser   LabelKind = "user"
)

// Label unifies Gmail labels, Zoho folders and labels, and IMAP folders. Exclusive carries
// folder semantics: applying an exclusive label moves the message out of wherever it was,
// while a non-exclusive one is added alongside. One flag lets a single modify path serve all
// three providers without any of them having to pretend to be another.
type Label struct {
	ID        LabelID
	Name      string
	Kind      LabelKind
	Exclusive bool
	Unread    int
	Total     int
}

// Query is the canonical search request. Raw carries provider-native syntax for callers who
// know which provider they are addressing; the structured fields are translated per provider
// and are the only ones safe to use across a fan-out.
type Query struct {
	Raw       string
	From      string
	To        string
	Subject   string
	Label     LabelID
	Unread    bool
	Starred   bool
	HasAttach bool
	After     time.Time
	Before    time.Time
	Limit     int
}

// Outgoing is a message to be sent or saved as a draft.
type Outgoing struct {
	Account     AccountID
	InReplyTo   ScopedID
	To          []Address
	Cc          []Address
	Bcc         []Address
	Subject     string
	Body        Body
	Attachments []Attachment
}

// Page is one page of results from a single account.
type Page[T any] struct {
	Items  []T
	Cursor string // empty when the account is exhausted
}
