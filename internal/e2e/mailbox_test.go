package e2e

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

// A mail server that records rather than sends.
//
// It is a fake rather than an httptest stub of a provider's REST API on purpose. What these
// tests need to know is what mailroom decided — whether a send reached a mailbox at all,
// whether the bytes attached to it were the bytes that were staged — and that is a question
// about the far end of the provider interface, not about the wire format of somebody's API.
// Stubbing at the HTTP layer would put a second implementation of Gmail between the assertion
// and the fact.

// mailbox is one linked account's worth of mail server. Every mutating call appends to a
// slice, and the tests read those slices.
type mailbox struct {
	mu sync.Mutex

	sent          []mail.Outgoing
	drafts        map[string]mail.Outgoing
	draftOrder    []string
	sentDrafts    []string
	deletedDrafts []string
	trashed       []string
	junked        []string
	untrashed     []string
	deleted       []string
	filters       []mail.Filter
	deletedFilter []string
	vacations     []mail.Vacation
	labelsAdded   []string
	flagsSet      []mail.FlagUpdate

	// messages and their attachments, so a read path has something to find.
	messages    map[string]mail.Message
	attachments map[string]map[string]mail.Attachment

	account mail.Account
	nextID  int
}

func newMailbox(acct mail.Account) *mailbox {
	return &mailbox{
		drafts:      map[string]mail.Outgoing{},
		messages:    map[string]mail.Message{},
		attachments: map[string]map[string]mail.Attachment{},
		account:     acct,
	}
}

// seed puts one message with one attachment in the mailbox, and answers with its scoped id.
func (m *mailbox) seed(subject, filename, mimeType string, content []byte) mail.ScopedID {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	native := fmt.Sprintf("msg%d", m.nextID)
	id := mail.ScopedID{Account: m.account.ID, Native: native}
	m.messages[native] = mail.Message{
		ID:       id,
		Account:  m.account.Alias,
		ThreadID: id,
		From:     mail.Address{Name: "A Sender", Email: "sender@example.net"},
		To:       []mail.Address{{Email: m.account.Address}},
		Subject:  subject,
		Date:     time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Body:     mail.Body{Text: "the body of " + subject},
		Attachments: []mail.AttachmentRef{{
			ID: "att1", Filename: filename, MimeType: mimeType, Size: int64(len(content)),
		}},
	}
	m.attachments[native] = map[string]mail.Attachment{
		"att1": {
			AttachmentRef: mail.AttachmentRef{
				ID: "att1", Filename: filename, MimeType: mimeType, Size: int64(len(content)),
			},
			Content: content,
		},
	}
	return id
}

func (m *mailbox) ID() mail.ProviderID    { return mail.ProviderIMAP }
func (m *mailbox) Capabilities() mail.Set { return mail.DerivedCapabilities(m) }
func (m *mailbox) Quirks() []mail.Quirk   { return nil }

// --- MessageReader ---

func (m *mailbox) Search(_ context.Context, q mail.Query, _ string) (mail.Page[mail.Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Message
	for _, msg := range m.messages {
		if q.Subject != "" && msg.Subject != q.Subject {
			continue
		}
		out = append(out, msg)
	}
	return mail.Page[mail.Message]{Items: out}, nil
}

func (m *mailbox) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.messages[id.Native]
	if !ok {
		return mail.Message{}, mail.ErrNotFound
	}
	return msg, nil
}

func (m *mailbox) GetThread(_ context.Context, id mail.ScopedID) (mail.Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.messages[id.Native]
	if !ok {
		return mail.Thread{}, mail.ErrNotFound
	}
	return mail.Thread{ID: id, Account: m.account.Alias, Subject: msg.Subject,
		Messages: []mail.Message{msg}}, nil
}

// --- AttachmentReader ---

func (m *mailbox) GetAttachment(_ context.Context, msg mail.ScopedID, attachmentID string) (mail.Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byID, ok := m.attachments[msg.Native]
	if !ok {
		return mail.Attachment{}, mail.ErrNotFound
	}
	att, ok := byID[attachmentID]
	if !ok {
		return mail.Attachment{}, mail.ErrNotFound
	}
	return att, nil
}

// --- MessageWriter ---

func (m *mailbox) Send(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, out)
	m.nextID++
	return mail.ScopedID{Account: m.account.ID, Native: fmt.Sprintf("sent%d", m.nextID)}, nil
}

// --- DraftManager ---

func (m *mailbox) CreateDraft(_ context.Context, out mail.Outgoing) (mail.ScopedID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	native := fmt.Sprintf("draft%d", m.nextID)
	m.drafts[native] = out
	m.draftOrder = append(m.draftOrder, native)
	return mail.ScopedID{Account: m.account.ID, Native: native}, nil
}

func (m *mailbox) UpdateDraft(_ context.Context, id mail.ScopedID, out mail.Outgoing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.drafts[id.Native]; !ok {
		return mail.ErrNotFound
	}
	m.drafts[id.Native] = out
	return nil
}

func (m *mailbox) SendDraft(_ context.Context, id mail.ScopedID) (mail.ScopedID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, ok := m.drafts[id.Native]
	if !ok {
		return mail.ScopedID{}, mail.ErrNotFound
	}
	delete(m.drafts, id.Native)
	m.sentDrafts = append(m.sentDrafts, id.Native)
	m.sent = append(m.sent, out)
	m.nextID++
	return mail.ScopedID{Account: m.account.ID, Native: fmt.Sprintf("sent%d", m.nextID)}, nil
}

func (m *mailbox) DeleteDraft(_ context.Context, id mail.ScopedID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.drafts[id.Native]; !ok {
		return mail.ErrNotFound
	}
	delete(m.drafts, id.Native)
	m.deletedDrafts = append(m.deletedDrafts, id.Native)
	return nil
}

func (m *mailbox) ListDrafts(context.Context, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

// --- LabelManager ---

func (m *mailbox) ListLabels(context.Context) ([]mail.Label, error) {
	return []mail.Label{{ID: "INBOX", Name: "Inbox", Kind: mail.LabelSystem}}, nil
}

func (m *mailbox) CreateLabel(_ context.Context, name string, _ bool) (mail.Label, error) {
	return mail.Label{ID: mail.LabelID(name), Name: name, Kind: mail.LabelUser}, nil
}

func (m *mailbox) DeleteLabel(context.Context, mail.LabelID) error { return nil }

// ApplyLabels mirrors what the real providers do with the label ids that are not ordinary
// labels.
//
// TRASH is trashing. Gmail's BatchModify moves a message to the bin when TRASH is added
// (internal/provider/gmail/write.go ApplyLabels), and the IMAP provider implements a label
// add as a MOVE into the named mailbox, so adding Trash there is the same operation as
// Trash() (internal/provider/imap/write.go ApplyLabels -> move). Junk is the same shape with
// a filter attached. Removing INBOX is archiving, which is what mail_modify's own `archive`
// flag compiles to.
//
// The fake honours all of it because a fake that quietly treated TRASH as a sticker would
// answer the question these tests exist to ask — what actually happens to somebody's mail —
// with this file's own opinion rather than with the product's behaviour. It classifies
// through the same model function the IMAP provider uses, so the fake cannot drift into
// disagreeing with what it is standing in for.
func (m *mailbox) ApplyLabels(_ context.Context, ids []mail.ScopedID, add, remove []mail.LabelID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		for _, l := range add {
			switch mail.EffectOfMailboxName(string(l)) {
			case mail.EffectTrash:
				m.trashed = append(m.trashed, id.Native)
			case mail.EffectSpam:
				m.junked = append(m.junked, id.Native)
			default:
				m.labelsAdded = append(m.labelsAdded, id.Native+"+"+string(l))
			}
		}
		for _, l := range remove {
			m.labelsAdded = append(m.labelsAdded, id.Native+"-"+string(l))
		}
	}
	return nil
}

// DeletingDestroysMail answers as the IMAP provider does: a label here is a mailbox, and
// deleting one takes the mail in it.
func (m *mailbox) DeletingDestroysMail(_ context.Context, _ mail.LabelID) (bool, error) {
	return true, nil
}

// EffectOfApplying answers as the IMAP provider does, because that is what this fake claims
// to be: an id here is a mailbox name, and moving mail into the one called Trash is the whole
// of trashing it.
func (m *mailbox) EffectOfApplying(_ context.Context, id mail.LabelID) (mail.LabelEffect, error) {
	return mail.EffectOfMailboxName(string(id)), nil
}

func (m *mailbox) SetFlags(_ context.Context, _ []mail.ScopedID, u mail.FlagUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flagsSet = append(m.flagsSet, u)
	return nil
}

// --- Destroyer ---

func (m *mailbox) Trash(_ context.Context, ids []mail.ScopedID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.trashed = append(m.trashed, id.Native)
	}
	return nil
}

func (m *mailbox) Untrash(_ context.Context, ids []mail.ScopedID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.untrashed = append(m.untrashed, id.Native)
	}
	return nil
}

func (m *mailbox) Delete(_ context.Context, ids []mail.ScopedID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.deleted = append(m.deleted, id.Native)
	}
	return nil
}

// --- FilterManager ---

func (m *mailbox) ListFilters(context.Context) ([]mail.Filter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Filter(nil), m.filters...), nil
}

func (m *mailbox) CreateFilter(_ context.Context, f mail.Filter) (mail.Filter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	f.ID = fmt.Sprintf("filter%d", m.nextID)
	m.filters = append(m.filters, f)
	return f, nil
}

func (m *mailbox) DeleteFilter(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedFilter = append(m.deletedFilter, id)
	return nil
}

// --- SettingsManager ---

func (m *mailbox) ListSendAs(context.Context) ([]mail.SendAs, error) {
	return []mail.SendAs{{Address: m.account.Address, Primary: true, Default: true, Verified: true}}, nil
}

func (m *mailbox) GetVacation(context.Context) (mail.Vacation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.vacations) == 0 {
		return mail.Vacation{}, nil
	}
	return m.vacations[len(m.vacations)-1], nil
}

func (m *mailbox) SetVacation(_ context.Context, v mail.Vacation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vacations = append(m.vacations, v)
	return nil
}

// --- reads, for assertions ---

func (m *mailbox) sentMessages() []mail.Outgoing {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Outgoing(nil), m.sent...)
}

func (m *mailbox) draftCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.drafts)
}

func (m *mailbox) snapshot() mailboxState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mailboxState{
		sends: len(m.sent), trashed: len(m.trashed), deleted: len(m.deleted),
		junked: len(m.junked), untrashed: len(m.untrashed), filters: len(m.filters),
		deletedFilters: len(m.deletedFilter), vacations: len(m.vacations),
		deletedDrafts: len(m.deletedDrafts),
	}
}

type mailboxState struct {
	sends, trashed, deleted, untrashed int
	junked                             int
	filters, deletedFilters, vacations int
	deletedDrafts                      int
}

// fleet is the provider factory the tool layer and the held queue are both built with.
type fleet struct {
	mu    sync.Mutex
	boxes map[mail.AccountID]*mailbox
}

func newFleet() *fleet { return &fleet{boxes: map[mail.AccountID]*mailbox{}} }

func (f *fleet) For(_ context.Context, acct mail.Account) (mail.Provider, error) {
	return f.box(acct), nil
}

func (f *fleet) box(acct mail.Account) *mailbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.boxes[acct.ID]; ok {
		return b
	}
	b := newMailbox(acct)
	f.boxes[acct.ID] = b
	return b
}
