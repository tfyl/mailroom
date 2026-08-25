package app

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/mail"
	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// liveConns counts what the server is holding open. The client end is the one that leaks, so
// the count has to be taken from the other side of the socket to mean anything.
type liveConns struct{ opened, closed atomic.Int64 }

func (l *liveConns) count() int64 { return l.opened.Load() - l.closed.Load() }

type countingListener struct {
	net.Listener
	live liveConns
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.live.opened.Add(1)
	return &countedConn{Conn: conn, live: &l.live}, nil
}

type countedConn struct {
	net.Conn
	live *liveConns
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.live.closed.Add(1) })
	return c.Conn.Close()
}

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

// linkedMailbox builds the whole path a tool call takes to a connection: a real database
// holding a sealed IMAP credential, and a real IMAP server at the other end. Nothing here is
// a stand-in, because what is being asserted is whether a socket exists.
func linkedMailbox(t *testing.T) (*Providers, mail.Account, *liveConns) {
	t.Helper()

	mem := imapmemserver.New()
	u := imapmemserver.NewUser("operator", "hunter2")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatalf("creating INBOX: %v", err)
	}
	mem.AddUser(u)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &countingListener{Listener: raw}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); _ = listener.Close() })

	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite://" + filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	owner, _, err := db.EnsureUser(ctx, user.User{Issuer: "test", Subject: "operator"},
		store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		t.Fatalf("creating the owner: %v", err)
	}

	acct := mail.Account{
		ID: "acct_imap", OwnerID: owner.ID, Alias: "imap", Address: "operator@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	blob, err := json.Marshal(imapprovider.Config{
		Host: listener.Addr().String(), Username: "operator", Password: "hunter2",
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.SealString(string(blob), string(acct.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LinkAccount(ctx, owner.ID, acct, sealed, ""); err != nil {
		t.Fatalf("linking the mailbox: %v", err)
	}

	providers := NewProviders(db, sealer, &config.Config{})
	// Long enough that a call in flight finishes, short enough to assert on.
	providers.grace = 20 * time.Millisecond
	return providers, acct, &listener.live
}

// expire ages a cache entry past the TTL, which is what a quiet five minutes does.
func expire(p *Providers, id mail.AccountID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.cache[id]
	c.built = c.built.Add(-2 * cacheTTL)
	p.cache[id] = c
}

// Every TTL boundary rebuilds the provider for a mailbox, and the one it replaced used to be
// dropped on the floor still holding its connection. mail_accounts builds one for every
// linked mailbox on every call, so an instance in ordinary use walked into the server's
// per-account connection limit — fifteen on Gmail, ten by default on Dovecot — within the
// hour, and the symptom arrived as a refused login rather than as anything about mailroom.
func TestRebuildingAProviderClosesTheOneItReplaced(t *testing.T) {
	providers, acct, live := linkedMailbox(t)
	ctx := context.Background()

	for round := range 5 {
		p, err := providers.For(ctx, acct)
		if err != nil {
			t.Fatalf("round %d: building a provider: %v", round, err)
		}
		if _, err := p.(mail.LabelManager).ListLabels(ctx); err != nil {
			t.Fatalf("round %d: the provider does not work: %v", round, err)
		}
		waitForConns(t, live, 1)
		expire(providers, acct.ID)
	}
}

// Forgetting a mailbox is what a re-link does. The cached provider becomes unreachable at
// that moment, so its connection has to go with it.
func TestForgettingAMailboxClosesItsProvider(t *testing.T) {
	providers, acct, live := linkedMailbox(t)
	ctx := context.Background()

	if _, err := providers.For(ctx, acct); err != nil {
		t.Fatalf("building a provider: %v", err)
	}
	waitForConns(t, live, 1)

	providers.Forget(acct.ID)
	waitForConns(t, live, 0)
}
