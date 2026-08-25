package imap

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// startServer runs an in-memory IMAP server for the duration of a test.
//
// This is what makes IMAP the first provider able to run the behavioural half of the
// conformance suite: no account, no credentials, no network. The suite finally gets to check
// behaviour rather than only structure.
//
// It counts connections as well, because whether this provider leaves them open is a
// property worth asserting rather than assuming, and only the server can see it.
func startServer(t *testing.T, messages int) (addr string, live *liveConns, user string, pass string) {
	t.Helper()

	raws := make([]string, 0, messages)
	for i := range messages {
		raws = append(raws, fmt.Sprintf(""+
			"From: Sender %[1]d <sender%[1]d@example.com>\r\n"+
			"To: operator@example.com\r\n"+
			"Subject: Test message %[1]d\r\n"+
			"Date: Mon, 0%[1]d Aug 2026 12:00:00 +0000\r\n"+
			"Message-ID: <msg%[1]d@example.com>\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Body of message %[1]d.\r\n", i+1))
	}
	return startServerWith(t, raws)
}

// startRecordingServer is startServer plus a handle on the transcript, for the tests that
// assert on the commands this provider sends rather than on what comes back.
func startRecordingServer(t *testing.T, messages int) (addr string, listener *countingListener, user, pass string) {
	t.Helper()
	addr, live, user, pass := startServer(t, messages)
	return addr, live.owner, user, pass
}

// startServerWith runs the server over messages given whole, for tests that care about a
// particular MIME shape rather than about how many messages there are.
func startServerWith(t *testing.T, raws []string) (addr string, live *liveConns, user string, pass string) {
	t.Helper()

	memServer := imapmemserver.New()
	u := imapmemserver.NewUser("operator", "hunter2")
	if err := u.Create("INBOX", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating INBOX: %v", err)
	}
	if err := u.Create("Trash", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating Trash: %v", err)
	}
	memServer.AddUser(u)

	for i, raw := range raws {
		if _, err := u.Append("INBOX", strings.NewReader(raw), &imap.AppendOptions{
			Time: time.Date(2026, 8, i+1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("appending message %d: %v", i, err)
		}
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})

	listenerRaw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &countingListener{Listener: listenerRaw}
	listener.live.owner = listener
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); _ = listener.Close() })

	return listener.Addr().String(), &listener.live, "operator", "hunter2"
}

// liveConns counts connections the server is holding: one up on accept, one down when the
// server closes it, which happens once the client has gone away.
type liveConns struct {
	opened, closed atomic.Int64
	// owner is the listener these counts belong to, so a helper handed the counts can still
	// reach the transcript beside them.
	owner *countingListener
}

func (l *liveConns) count() int64 { return l.opened.Load() - l.closed.Load() }

type countingListener struct {
	net.Listener
	live liveConns
	// sent accumulates everything the client wrote, which is what makes the commands this
	// provider issues assertable rather than inferred from the criteria struct it built.
	// A struct is what mailroom meant; the command line is what the server was asked.
	mu   sync.Mutex
	sent []byte
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.live.opened.Add(1)
	return &countedConn{Conn: conn, live: &l.live, listener: l}, nil
}

// commands returns everything the client has sent so far, as text.
func (l *countingListener) commands() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.sent)
}

type countedConn struct {
	net.Conn
	live     *liveConns
	listener *countingListener
	once     sync.Once
}

// Read tees what the client sent into the listener's transcript. The server reads from this
// end, so everything the client wrote passes through here exactly once.
func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.listener != nil {
		c.listener.mu.Lock()
		c.listener.sent = append(c.listener.sent, b[:n]...)
		c.listener.mu.Unlock()
	}
	return n, err
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.live.closed.Add(1) })
	return c.Conn.Close()
}

// waitForConns waits for the server to settle on a connection count. A client closing its
// end is not instantaneous from the server's side, so the assertion has to be given a moment
// rather than read once.
func waitForConns(t *testing.T, live *liveConns, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := live.count()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d connection(s) open, got %d", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTestProvider(t *testing.T, messages int) *Provider {
	t.Helper()
	addr, _, user, pass := startServer(t, messages)

	p, err := New(context.Background(), mmail.Account{
		ID: "acct_imap", Alias: "imap", Address: "operator@example.com",
		Provider: mmail.ProviderIMAP, Status: mmail.StatusLinked,
	}, Config{Host: addr, Username: user, Password: pass, TLS: false})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestIMAPStaticConformance(t *testing.T) {
	conformance.Static(t, newTestProvider(t, 0))
}

// The first behavioural run of the contract by any provider.
func TestIMAPLiveConformance(t *testing.T) {
	p := newTestProvider(t, 5)

	conformance.Live(t, conformance.Harness{
		Provider:  p,
		Account:   mmail.Account{ID: "acct_imap", Alias: "imap", Address: "operator@example.com"},
		SearchAll: mmail.Query{Limit: 10},
		// Well-formed for this provider — a real mailbox and a uid that was never issued.
		MissingID: mmail.ScopedID{Account: "acct_imap", Native: "INBOX/999999"},
	})
}

// Sending needs SMTP, and this account has none. The capability must be withheld rather than
// advertised and then failed at the moment someone tries to use it.
func TestIMAPWithholdsSendWithoutSMTP(t *testing.T) {
	p := newTestProvider(t, 0)

	if p.Capabilities().Has(mmail.CapSend) {
		t.Error("send must not be advertised when no SMTP host is configured")
	}
	if _, ok := any(p).(mmail.MessageWriter); !ok {
		t.Error("the interface must still be implemented; only the claim is withheld")
	}

	_, err := p.Send(context.Background(), mmail.Outgoing{To: []mmail.Address{{Email: "a@example.com"}}})
	var unsupported *mmail.UnsupportedError
	if !asUnsupported(err, &unsupported) {
		t.Errorf("want UnsupportedError, got %T: %v", err, err)
	}
}

// Every mailbox is exclusive here, which is the opposite of Gmail. A non-exclusive label has
// to be refused rather than approximated with something that behaves differently.
func TestIMAPRefusesNonExclusiveLabels(t *testing.T) {
	p := newTestProvider(t, 0)

	if _, err := p.CreateLabel(context.Background(), "should-not-exist", false); err == nil {
		t.Fatal("a non-exclusive label must be refused")
	}

	label, err := p.CreateLabel(context.Background(), "Projects", true)
	if err != nil {
		t.Fatalf("creating a mailbox failed: %v", err)
	}
	if !label.Exclusive {
		t.Error("an IMAP mailbox is always exclusive")
	}
}

func TestIMAPIDsCarryMailboxAndUID(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1"}}

	id := p.scoped("INBOX/Archive", 42)
	mailbox, uid, err := splitNative(id.Native)
	if err != nil {
		t.Fatalf("an id this provider produced was not parseable by it: %v", err)
	}
	// Mailbox names contain slashes as hierarchy separators, so the split must take the last
	// one rather than the first.
	if mailbox != "INBOX/Archive" || uid != 42 {
		t.Errorf("round trip lost data: mailbox=%q uid=%d", mailbox, uid)
	}
	if _, _, err := splitNative("42"); err == nil {
		t.Error("an id without a mailbox must be refused")
	}
}

func asUnsupported(err error, target **mmail.UnsupportedError) bool {
	if err == nil {
		return false
	}
	if u, ok := err.(*mmail.UnsupportedError); ok {
		*target = u
		return true
	}
	return false
}

// A SELECT can fail because the connection died, which a reconnect fixes, or because the
// mailbox is not there, which it does not. Both used to reconnect, and the connection being
// replaced was dropped rather than closed — so a caller asking for a folder that does not
// exist left a live connection behind on every call. Servers count those: Gmail allows
// fifteen at once, Dovecot ten per address by default.
func TestSelectingAMissingMailboxLeavesNoConnectionBehind(t *testing.T) {
	addr, live, user, pass := startServer(t, 0)

	p, err := New(context.Background(), mmail.Account{ID: "acct_imap", Alias: "imap"},
		Config{Host: addr, Username: user, Password: pass})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	for i := range 5 {
		if _, err := p.Get(context.Background(),
			mmail.ScopedID{Account: "acct_imap", Native: "NoSuchMailbox/1"}); err == nil {
			t.Fatalf("round %d: fetching from a mailbox that does not exist must fail", i)
		}
		waitForConns(t, live, 1)
	}
}

// Close means the provider has been released and nothing can reach it again. Reconnecting
// after that would open a connection with no owner and no second chance to close it, which
// is the leak this all exists to prevent — so it has to refuse rather than quietly work.
func TestAReleasedProviderDoesNotReconnect(t *testing.T) {
	addr, live, user, pass := startServer(t, 1)

	p, err := New(context.Background(), mmail.Account{ID: "acct_imap", Alias: "imap"},
		Config{Host: addr, Username: user, Password: pass})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	waitForConns(t, live, 0)

	if _, err := p.ListLabels(context.Background()); err == nil {
		t.Error("a released provider must refuse rather than open a fresh connection")
	}
	waitForConns(t, live, 0)
}
