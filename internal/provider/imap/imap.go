// Package imap implements the mail provider interfaces against any IMAP server, with SMTP
// for sending.
//
// It is the widest departure from the hosted providers, which is why it exists: folders
// rather than labels, so every placement is exclusive; no thread ids, so conversations must
// be derived from headers; no filters or settings at all. A seam that survives Gmail, Zoho
// and this is a seam worth trusting.
package imap

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Config describes how to reach a server.
type Config struct {
	Host     string // host:port for IMAP
	Username string
	Password string
	TLS      bool

	SMTPHost string // host:port for sending; empty disables Send
	SMTPFrom string

	// Credentials for SMTP, when they differ from the IMAP ones. Empty means reuse the
	// IMAP username and password, which is the common case and the whole point of an
	// app password.
	SMTPUsername string
	SMTPPassword string
}

// Provider talks to one IMAP account.
//
// An IMAP connection is stateful in a way HTTP APIs are not: SELECT sets a current mailbox
// that subsequent commands operate on. Two goroutines sharing a connection would silently
// read from each other's mailbox, so every operation holds the mutex for its whole
// select-and-act sequence. This is the reason the provider declares no_batch — throughput
// here comes from doing less, not from more concurrency.
type Provider struct {
	mu      sync.Mutex
	client  *imapclient.Client
	closed  bool
	cfg     Config
	account mmail.Account
}

func New(ctx context.Context, account mmail.Account, cfg Config) (*Provider, error) {
	p := &Provider{cfg: cfg, account: account}
	if err := p.connect(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) connect() error {
	if p.closed {
		// Reconnecting here would produce a connection nothing owns: the cache released this
		// provider precisely because nothing can reach it any more, so there would be no
		// second chance to close it.
		return p.wrap("connect", errors.New("this provider has been released"))
	}
	if p.client != nil {
		// The connection being replaced is closed rather than logged out. connect is reached
		// when the current one has stopped answering, and LOGOUT waits for a reply.
		_ = p.client.Close()
		p.client = nil
	}

	var (
		client *imapclient.Client
		err    error
	)
	if p.cfg.TLS {
		client, err = imapclient.DialTLS(p.cfg.Host, nil)
	} else {
		client, err = imapclient.DialInsecure(p.cfg.Host, nil)
	}
	if err != nil {
		return p.wrap("connect", err)
	}
	if err := client.Login(p.cfg.Username, p.cfg.Password).Wait(); err != nil {
		_ = client.Close()
		// A rejected login is a credential problem an operator has to fix, not something a
		// retry will resolve.
		return mmail.ErrNeedsReauth
	}
	p.client = client
	return nil
}

func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	client := p.client
	p.client = nil
	if client == nil {
		return nil
	}
	return client.Logout().Wait()
}

func (p *Provider) ID() mmail.ProviderID { return mmail.ProviderIMAP }

// Capabilities starts from the interfaces implemented and then withholds what this
// particular configuration cannot actually do.
//
// Sending needs an SMTP host. The method exists either way — it has to, for the interface —
// but advertising send on an account with no SMTP configured would promise something that
// fails at the worst possible moment. Claiming less than is implemented is allowed by the
// contract; claiming more is not.
func (p *Provider) Capabilities() mmail.Set {
	caps := mmail.DerivedCapabilities(p)
	if p.cfg.SMTPHost == "" {
		delete(caps, mmail.CapSend)
	}
	return caps
}

// Quirks tells callers everything about this provider that changes how results should be
// read. All four apply, which is what makes it a useful third implementation.
func (p *Provider) Quirks() []mmail.Quirk {
	return []mmail.Quirk{
		mmail.QuirkDerivedThreads,
		mmail.QuirkExclusiveLabel,
		mmail.QuirkNoBatch,
		mmail.QuirkPartialSearch,
	}
}

// --- addressing ---
//
// A message is identified by mailbox and UID. UIDs are only unique within a mailbox, so the
// mailbox has to travel with them — the same shape Zoho needs for a different reason, and
// invisible to everything above this package.

func (p *Provider) scoped(mailbox string, uid imap.UID) mmail.ScopedID {
	return mmail.ScopedID{
		Account: p.account.ID,
		Native:  mailbox + "/" + strconv.FormatUint(uint64(uid), 10),
	}
}

func splitNative(native string) (mailbox string, uid imap.UID, err error) {
	// Mailbox names may contain slashes as hierarchy separators, so split on the last one.
	idx := strings.LastIndex(native, "/")
	if idx <= 0 || idx == len(native)-1 {
		return "", 0, fmt.Errorf("malformed imap id %q: want <mailbox>/<uid>", native)
	}
	n, err := strconv.ParseUint(native[idx+1:], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("malformed imap uid in %q", native)
	}
	return native[:idx], imap.UID(n), nil
}

// selectMailbox switches the connection to a mailbox, reconnecting once if the server has
// dropped the connection — long-lived IMAP connections are routinely closed by servers.
func (p *Provider) selectMailbox(name string, readOnly bool) error {
	if p.client == nil {
		if err := p.connect(); err != nil {
			return err
		}
	}
	_, err := p.client.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err == nil {
		return nil
	}

	if reconnectErr := p.connect(); reconnectErr != nil {
		return p.wrap("select", err)
	}
	if _, err := p.client.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait(); err != nil {
		return p.wrap("select", err)
	}
	return nil
}

func (p *Provider) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authenticationfailed"), strings.Contains(msg, "invalid credentials"):
		return mmail.ErrNeedsReauth
	case strings.Contains(msg, "nonexistent"), strings.Contains(msg, "no such"):
		return mmail.ErrNotFound
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"), strings.Contains(msg, "eof"):
		// Connection-level failures are worth retrying; a fresh connection usually succeeds.
		return &mmail.ProviderError{
			Provider: mmail.ProviderIMAP, Account: p.account.Alias,
			Address: p.account.Address, Op: op,
			Retryable: true, Err: err,
		}
	}
	return &mmail.ProviderError{
		Provider: mmail.ProviderIMAP, Account: p.account.Alias,
		Address: p.account.Address, Op: op, Err: err,
	}
}

// unsupported names one operation this provider cannot perform, rather than the capability
// containing it.
//
// IMAP withholds narrow slices of capabilities it otherwise serves — a folder can be created
// but a label cannot, a message can be moved but not unfiled, mail can be searched but not by
// whether it carries an attachment. Reporting any of those as "labels are unsupported" or
// "read is unsupported" would tell a caller to stop attempting the neighbouring operations
// that work perfectly.
func (p *Provider) unsupported(capability mmail.Capability, op, reason string) error {
	return &mmail.UnsupportedError{
		Provider:   mmail.ProviderIMAP,
		Account:    p.account.Alias,
		Address:    p.account.Address,
		Capability: capability,
		Op:         op,
		Reason:     reason,
	}
}

const defaultMailbox = "INBOX"
