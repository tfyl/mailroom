package imap

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/rfc5322"
)

// --- LabelManager: IMAP folders ---

// ListLabels reports mailboxes. Every one is exclusive, because a message lives in exactly
// one folder — the opposite of Gmail, and the reason the model carries the flag rather than
// assuming either shape.
func (p *Provider) ListLabels(ctx context.Context) ([]mmail.Label, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client == nil {
		if err := p.connect(); err != nil {
			return nil, err
		}
	}

	cmd := p.client.List("", "*", &imap.ListOptions{})
	boxes, err := cmd.Collect()
	if err != nil {
		return nil, p.wrap("list_labels", err)
	}

	out := make([]mmail.Label, 0, len(boxes))
	for _, b := range boxes {
		kind := mmail.LabelUser
		if isSystemMailbox(b.Mailbox) {
			kind = mmail.LabelSystem
		}
		out = append(out, mmail.Label{
			ID:        mmail.LabelID(b.Mailbox),
			Name:      b.Mailbox,
			Kind:      kind,
			Exclusive: true,
		})
	}
	return out, nil
}

// CreateLabel makes a mailbox. A non-exclusive label is refused rather than approximated:
// IMAP keywords exist but do not behave like labels across servers, and handing back
// something that behaves differently from what was asked for is worse than saying no.
func (p *Provider) CreateLabel(ctx context.Context, name string, exclusive bool) (mmail.Label, error) {
	if !exclusive {
		// Named as one operation, not as the whole capability: creating a mailbox works, and
		// a caller told "labels are unsupported here" would stop asking for the thing it can
		// have.
		return mmail.Label{}, p.unsupported(mmail.CapLabels, "creating a non-exclusive label",
			"every IMAP mailbox is exclusive, so a label that sits alongside the message's "+
				"placement has nothing to map onto; IMAP keywords are the nearest thing and "+
				"they do not behave alike across servers")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		if err := p.connect(); err != nil {
			return mmail.Label{}, err
		}
	}
	if err := p.client.Create(name, nil).Wait(); err != nil {
		return mmail.Label{}, p.wrap("create_label", err)
	}
	return mmail.Label{ID: mmail.LabelID(name), Name: name, Kind: mmail.LabelUser, Exclusive: true}, nil
}

func (p *Provider) DeleteLabel(ctx context.Context, id mmail.LabelID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		if err := p.connect(); err != nil {
			return err
		}
	}
	return p.wrap("delete_label", p.client.Delete(string(id)).Wait())
}

// ApplyLabels moves messages between mailboxes.
//
// Applying an exclusive label is a move, so at most one may be applied. A removal is refused
// rather than skipped, which is the change worth explaining: a message is always in exactly
// one mailbox and never out of all of them, so there is nothing "remove this label" could do
// here — and returning success for it told the caller a mailbox had been changed when
// nothing had happened at all. Every archive, and every attempt to unlabel, came back ok.
//
// Refusing by name leaves the caller a request that works: name the destination mailbox in
// the labels to add, and the message moves.
func (p *Provider) ApplyLabels(ctx context.Context, ids []mmail.ScopedID, add, remove []mmail.LabelID) error {
	if len(ids) == 0 {
		return nil
	}
	if len(remove) > 0 {
		return p.unsupported(mmail.CapLabels, "removing a label",
			"a message here lives in exactly one mailbox and is never in none of them, so "+
				"there is no removal to perform; move it instead, by naming the destination "+
				"mailbox as the label to add")
	}
	if len(add) == 0 {
		return nil
	}
	if len(add) > 1 {
		return p.unsupported(mmail.CapLabels, "applying more than one label at once",
			fmt.Sprintf("a message can only be in one mailbox; this asked to move it to %d", len(add)))
	}
	return p.move(ids, string(add[0]))
}

func (p *Provider) move(ids []mmail.ScopedID, dest string) error {
	grouped, err := groupByMailbox(ids)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for mailbox, uids := range grouped {
		if mailbox == dest {
			continue
		}
		if err := p.selectMailbox(mailbox, false); err != nil {
			return err
		}
		if _, err := p.client.Move(imap.UIDSetNum(uids...), dest).Wait(); err != nil {
			return p.wrap("move", err)
		}
	}
	return nil
}

// SetFlags writes \\Seen and \\Flagged, and only the ones the update names.
//
// The delta matters more here than anywhere else, because IMAP has no way to say "set these
// two and leave the rest": a STORE either adds a flag or removes it. An update that named
// both every time would clear \\Flagged on every message somebody marked read.
func (p *Provider) SetFlags(ctx context.Context, ids []mmail.ScopedID, update mmail.FlagUpdate) error {
	if len(ids) == 0 || update.Empty() {
		return nil
	}
	grouped, err := groupByMailbox(ids)
	if err != nil {
		return err
	}

	add, remove := flagChanges(update)

	p.mu.Lock()
	defer p.mu.Unlock()
	for mailbox, uids := range grouped {
		if err := p.selectMailbox(mailbox, false); err != nil {
			return err
		}
		set := imap.UIDSetNum(uids...)

		if err := p.store(set, imap.StoreFlagsAdd, add); err != nil {
			return err
		}
		if err := p.store(set, imap.StoreFlagsDel, remove); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) store(set imap.NumSet, op imap.StoreFlagsOp, flags []imap.Flag) error {
	if len(flags) == 0 {
		return nil
	}
	cmd := p.client.Store(set, &imap.StoreFlags{Op: op, Flags: flags, Silent: true}, nil)
	return p.wrap("store", cmd.Close())
}

// flagChanges splits an update into the flags to add and the flags to remove. A field the
// update left nil appears in neither list, so nothing is written for it.
func flagChanges(update mmail.FlagUpdate) (add, remove []imap.Flag) {
	for _, change := range []struct {
		wanted *bool
		flag   imap.Flag
	}{
		{update.Read, imap.FlagSeen},
		{update.Starred, imap.FlagFlagged},
	} {
		switch {
		case change.wanted == nil:
		case *change.wanted:
			add = append(add, change.flag)
		default:
			remove = append(remove, change.flag)
		}
	}
	return add, remove
}

// --- Destroyer ---

const trashMailbox = "Trash"

func (p *Provider) Trash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.move(ids, trashMailbox)
}

func (p *Provider) Untrash(ctx context.Context, ids []mmail.ScopedID) error {
	return p.move(ids, defaultMailbox)
}

// Delete sets \Deleted and expunges. There is no undo, which is why this needs the
// destructive capability rather than labels.
func (p *Provider) Delete(ctx context.Context, ids []mmail.ScopedID) error {
	grouped, err := groupByMailbox(ids)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for mailbox, uids := range grouped {
		if err := p.selectMailbox(mailbox, false); err != nil {
			return err
		}
		set := imap.UIDSetNum(uids...)
		if err := p.store(set, imap.StoreFlagsAdd, []imap.Flag{imap.FlagDeleted}); err != nil {
			return err
		}
		if err := p.client.Expunge().Close(); err != nil {
			return p.wrap("expunge", err)
		}
	}
	return nil
}

// --- MessageWriter: SMTP ---

// Send delivers over SMTP, reusing the shared RFC 5322 composer.
//
// Sending is only offered when an SMTP host is configured; see Capabilities, which does not
// advertise it otherwise.
func (p *Provider) Send(ctx context.Context, out mmail.Outgoing) (mmail.ScopedID, error) {
	if p.cfg.SMTPHost == "" {
		return mmail.ScopedID{}, &mmail.UnsupportedError{
			Provider: mmail.ProviderIMAP, Account: p.account.Alias,
			Address: p.account.Address, Capability: mmail.CapSend,
		}
	}

	from := p.cfg.SMTPFrom
	if from == "" {
		from = p.account.Address
	}

	raw, err := rfc5322.Compose(out, from, nil)
	if err != nil {
		return mmail.ScopedID{}, fmt.Errorf("composing message: %w", err)
	}

	recipients := make([]string, 0, len(out.To)+len(out.Cc)+len(out.Bcc))
	for _, group := range [][]mmail.Address{out.To, out.Cc, out.Bcc} {
		for _, a := range group {
			recipients = append(recipients, a.Email)
		}
	}
	if len(recipients) == 0 {
		return mmail.ScopedID{}, fmt.Errorf("no recipients")
	}

	if err := smtp.SendMail(p.cfg.SMTPHost, p.smtpAuth(), from, recipients, strings.NewReader(string(raw))); err != nil {
		return mmail.ScopedID{}, p.wrap("send", err)
	}

	// SMTP returns no identifier, and the message is not in any mailbox this account can
	// address. Reporting an id that resolves to nothing would be worse than reporting none.
	return mmail.ScopedID{}, nil
}

// smtpAuth builds the credentials for sending, reusing the IMAP ones unless separate SMTP
// ones were configured.
//
// Nil when there is nothing to authenticate with, which is right for a relay on a private
// network that accepts mail from its own hosts and wrong everywhere else: sending was
// previously hard-coded to nil, so it could never work against any server that asks who you
// are, Gmail included.
func (p *Provider) smtpAuth() sasl.Client {
	username, password := p.cfg.SMTPUsername, p.cfg.SMTPPassword
	if username == "" && password == "" {
		username, password = p.cfg.Username, p.cfg.Password
	}
	if username == "" || password == "" {
		return nil
	}
	// SendMail issues STARTTLS before authenticating whenever the server advertises it, so
	// the password does not cross a public network in clear.
	return sasl.NewPlainClient("", username, password)
}

func groupByMailbox(ids []mmail.ScopedID) (map[string][]imap.UID, error) {
	out := map[string][]imap.UID{}
	for _, id := range ids {
		mailbox, uid, err := splitNative(id.Native)
		if err != nil {
			return nil, err
		}
		out[mailbox] = append(out[mailbox], uid)
	}
	return out, nil
}

func isSystemMailbox(name string) bool {
	switch strings.ToUpper(name) {
	case "INBOX", "SENT", "DRAFTS", "TRASH", "JUNK", "SPAM", "ARCHIVE":
		return true
	}
	return false
}

// EffectOfApplying classifies an IMAP mailbox name.
//
// Applying a label here is a MOVE into the named mailbox, so moving a message into Trash is
// literally the call Trash makes above — same destination, same command, one of them gated on
// the destructive capability and the other, until this existed, on labels alone.
//
// The classification is by name, which is the only thing an id is here. That leaves a mailbox
// whose bin is called Papierkorb reading as an ordinary folder; the honest fix is the
// SPECIAL-USE attributes from an extended LIST, which are authoritative and locale-proof, and
// which cost a round trip per modify against a server that may not implement the extension at
// all. Names cover the mailboxes every mainstream client creates, and the gap is worth writing
// down rather than papering over.
// DeletingDestroysMail is true for every IMAP label, because an IMAP label is a mailbox.
//
// DELETE removes the mailbox and every message in it, and RFC 3501 gives it no undo: there is
// no bin to recover from, unlike the folder providers where a deleted folder's mail is at
// least somewhere. It is the most destructive single call this provider exposes.
func (p *Provider) DeletingDestroysMail(_ context.Context, _ mmail.LabelID) (bool, error) {
	return true, nil
}

func (p *Provider) EffectOfApplying(_ context.Context, id mmail.LabelID) (mmail.LabelEffect, error) {
	return mmail.EffectOfMailboxName(string(id)), nil
}
