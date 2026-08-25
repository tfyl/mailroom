package oauthsrv

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/mcp"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// rig is a mailroom cut down to the parts a bearer token passes through: a store holding one
// grant, this authorization server, and the MCP endpoint that resolves tokens against it.
// The clock is the rig's own so the coarsening window can be crossed without waiting.
type rig struct {
	db       *store.Store
	oauth    *Server
	endpoint *httptest.Server
	path     string
	owner    user.ID
	grant    grant.ID
	token    string
	now      time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open("sqlite://" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	me, _, err := db.EnsureUser(ctx, user.User{
		Issuer: "https://idp.example.com", Subject: "ada", Email: "ada@example.com",
	}, store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterClient(ctx, store.Client{ID: "client_1", Name: "An agent"}); err != nil {
		t.Fatal(err)
	}
	account := mail.Account{
		ID: "acct_1", Alias: "work", Address: "ada@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(ctx, me.ID, account, "sealed", ""); err != nil {
		t.Fatal(err)
	}
	g := &grant.Grant{
		ID: "grant_1", OwnerID: me.ID, ClientID: "client_1", Label: "An agent",
		Accounts: []mail.AccountID{account.ID}, Caps: mail.NewSet(mail.CapRead),
	}
	if err := db.CreateGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := db.IssueToken(ctx, "a-real-bearer-token", g.ID, nil); err != nil {
		t.Fatal(err)
	}

	r := &rig{
		db: db, path: path, owner: me.ID, grant: g.ID,
		token: "a-real-bearer-token",
		now:   time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}
	r.oauth = New(db, "https://mail.example.com")
	r.oauth.now = func() time.Time { return r.now }

	srv := mcp.NewServer(r.oauth, mcp.NewTools(nil, nil, nil), "https://mail.example.com",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.endpoint = httptest.NewServer(srv.Handler())
	t.Cleanup(r.endpoint.Close)
	return r
}

// bearer presents the token the way a real MCP client does.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// use makes a real MCP request over the SDK's own client, which is what puts the token
// through the bearer middleware and out the other side into GrantForRequest.
func (r *rig) use(t *testing.T) (*sdk.ClientSession, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             r.endpoint.URL,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: r.token, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, nil
}

func (r *rig) mustUse(t *testing.T) {
	t.Helper()
	if _, err := r.use(t); err != nil {
		t.Fatalf("an ordinary MCP request should have gone through: %v", err)
	}
}

func (r *rig) lastUsed(t *testing.T) *time.Time {
	t.Helper()
	g, err := r.db.Grant(context.Background(), r.grant)
	if err != nil {
		t.Fatal(err)
	}
	return g.LastUsedAt
}

// The bug: TouchGrant existed and nothing called it, so last_used_at was written nowhere and
// every grant read as never used however hard its client was working.
func TestAGrantUsedOverMCPRecordsItsLastUse(t *testing.T) {
	r := newRig(t)

	if used := r.lastUsed(t); used != nil {
		t.Fatalf("a grant nothing has presented has no last use, got %v", used)
	}

	r.mustUse(t)

	used := r.lastUsed(t)
	if used == nil {
		t.Fatal("an MCP request carrying this grant left no last use behind")
	}
	if !used.Equal(r.now) {
		t.Fatalf("last use is %v, want %v", used, r.now)
	}
}

func TestALaterUseReplacesTheRecordedTime(t *testing.T) {
	r := newRig(t)
	r.mustUse(t)

	r.now = r.now.Add(time.Hour)
	r.mustUse(t)

	if used := r.lastUsed(t); used == nil || !used.Equal(r.now) {
		t.Fatalf("last use is %v, want the later use at %v", used, r.now)
	}
}

// Every MCP request passes through the resolver and MCP clients poll, so the write is
// coarsened: within the window the stored value stands, and past it the next request replaces
// it. The page renders last use to the minute, so nothing finer would ever be seen.
func TestUsesInsideTheWindowAreWrittenOnce(t *testing.T) {
	r := newRig(t)
	first := r.now

	r.mustUse(t)

	r.now = first.Add(grant.TouchInterval - time.Second)
	r.mustUse(t)
	if used := r.lastUsed(t); used == nil || !used.Equal(first) {
		t.Fatalf("a use inside the window rewrote the value: got %v, want %v", used, first)
	}

	r.now = first.Add(grant.TouchInterval + time.Second)
	r.mustUse(t)
	if used := r.lastUsed(t); used == nil || !used.Equal(r.now) {
		t.Fatalf("a use past the window did not refresh the value: got %v, want %v", used, r.now)
	}
}

// Used means presented, not authorised. A client still calling with a grant its operator has
// revoked is exactly what somebody reading the grants page wants to see, and it is the case a
// capability-level hook would have shown as dormant.
func TestARevokedGrantStillRecordsThatItWasPresented(t *testing.T) {
	r := newRig(t)
	if err := r.db.RevokeGrant(context.Background(), r.owner, r.grant); err != nil {
		t.Fatal(err)
	}

	if _, err := r.use(t); err == nil {
		t.Fatal("a revoked grant must not be able to open a session")
	}

	if used := r.lastUsed(t); used == nil || !used.Equal(r.now) {
		t.Fatalf("the refused request should still count as use, got %v", used)
	}
}

// Last use is a display. A database that will not take the write is a cosmetic problem, and
// turning it into a failed mail call would be a real one.
func TestAGrantThatCannotBeRecordedStillServesTheRequest(t *testing.T) {
	r := newRig(t)

	// A second connection to the same file, so the refusal comes from SQLite on the real
	// write path rather than from a stub standing in for it.
	raw, err := sql.Open("sqlite", r.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_touch BEFORE UPDATE OF last_used_at ON grants
		BEGIN SELECT RAISE(ABORT, 'last_used_at is not writable'); END`); err != nil {
		t.Fatal(err)
	}

	// The failure is logged, and the log line is not what is under test here.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	session, err := r.use(t)
	if err != nil {
		t.Fatalf("a failed last-use write must not fail the MCP request: %v", err)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("the session should still serve calls: %v", err)
	}
	if used := r.lastUsed(t); used != nil {
		t.Fatalf("the write was refused, so nothing should have been recorded: %v", used)
	}

	// And with the refusal lifted the same rig records a use, so what the assertion above
	// saw was a database declining the write rather than nothing having tried to make it.
	if _, err := raw.Exec(`DROP TRIGGER refuse_touch`); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Hour)
	r.mustUse(t)
	if used := r.lastUsed(t); used == nil || !used.Equal(r.now) {
		t.Fatalf("last use is %v, want %v", used, r.now)
	}
}
