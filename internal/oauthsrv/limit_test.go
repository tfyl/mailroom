package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// registrar is this authorization server with a real database behind it, which is what the
// bound is protecting: the interesting assertion is not only the status code but that a
// refused registration left no row.
type registrar struct {
	server *Server
	db     *store.Store
	mux    *http.ServeMux
}

func newRegistrar(t *testing.T, proxies []string, perAddress, instance int) *registrar {
	t.Helper()

	db, err := store.Open("sqlite://" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	trusted, err := auth.ParseTrustedProxies(proxies)
	if err != nil {
		t.Fatal(err)
	}

	s := New(db, "https://mail.example.com").
		WithRegistrationLimit(trusted, perAddress, time.Hour, instance, time.Hour)
	mux := http.NewServeMux()
	s.Routes(mux)
	return &registrar{server: s, db: db, mux: mux}
}

// register posts one registration and answers with the response.
func (r *registrar) register(t *testing.T, remoteAddr string, forwarded string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"client_name":"An agent","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	return w
}

// clientID reads the id out of a successful registration, and fails if there was not one.
func clientID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("registration should have succeeded, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ClientID == "" {
		t.Fatal("a successful registration must answer with a client_id")
	}
	return body.ClientID
}

// The flow that has to keep working. A desktop MCP client introducing itself, and the same
// person doing it again after a reinstall, are both well inside any bound worth having.
func TestALegitimateRegistrationStillSucceeds(t *testing.T) {
	r := newRegistrar(t, nil, 20, 200)

	for i := range 5 {
		id := clientID(t, r.register(t, "198.51.100.7:5555", ""))
		if _, err := r.db.Client(context.Background(), id); err != nil {
			t.Fatalf("registration %d wrote no client row: %v", i, err)
		}
	}
}

func TestRegistrationIsRefusedPastThePerAddressLimit(t *testing.T) {
	r := newRegistrar(t, nil, 3, 100)

	for i := range 3 {
		if w := r.register(t, "198.51.100.7:5555", ""); w.Code != http.StatusCreated {
			t.Fatalf("registration %d should have been allowed, got %d", i, w.Code)
		}
	}

	w := r.register(t, "198.51.100.7:5555", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth registration should have been refused, got %d", w.Code)
	}
	assertRefusal(t, w)

	// And a caller in front of the bound still has its whole allowance.
	if w := r.register(t, "203.0.113.4:5555", ""); w.Code != http.StatusCreated {
		t.Fatalf("a different address should be unaffected, got %d", w.Code)
	}
}

// The trap this is here to avoid. With no trusted proxy configured, a header naming somebody
// else is worth nothing, so a caller cannot mint a fresh allowance by writing a new address
// into it.
func TestAnUntrustedClientCannotSpoofItsAddress(t *testing.T) {
	r := newRegistrar(t, nil, 3, 100)

	for i := range 3 {
		if w := r.register(t, "198.51.100.7:5555", "10.0.0.1"); w.Code != http.StatusCreated {
			t.Fatalf("registration %d should have been allowed, got %d", i, w.Code)
		}
	}
	for _, claimed := range []string{"192.0.2.1", "203.0.113.9", "198.51.100.8", "127.0.0.1"} {
		w := r.register(t, "198.51.100.7:5555", claimed)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("X-Forwarded-For %q from an untrusted source must not buy a new allowance, got %d",
				claimed, w.Code)
		}
	}
}

// The same header from a source that is on the list is the deployment this exists for: behind
// Cloudflare Access or a local cloudflared, two different people must not share one bucket.
func TestATrustedProxySeparatesItsClients(t *testing.T) {
	r := newRegistrar(t, []string{"127.0.0.1/32"}, 3, 100)

	for i := range 3 {
		if w := r.register(t, "127.0.0.1:9000", "198.51.100.7"); w.Code != http.StatusCreated {
			t.Fatalf("registration %d should have been allowed, got %d", i, w.Code)
		}
	}
	if w := r.register(t, "127.0.0.1:9000", "198.51.100.7"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the client past its allowance should have been refused, got %d", w.Code)
	}
	if w := r.register(t, "127.0.0.1:9000", "203.0.113.4"); w.Code != http.StatusCreated {
		t.Fatalf("a different client behind the same proxy must have its own allowance, got %d", w.Code)
	}
	// A client that writes its own entry before the proxy appends what it saw is still
	// attributed to what the proxy saw.
	if w := r.register(t, "127.0.0.1:9000", "203.0.113.99, 198.51.100.7"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a forged prefix must not buy a new allowance, got %d", w.Code)
	}
}

// A botnet walks past a per-address bound by definition, which is why there is a second one.
func TestTheInstanceBoundRefusesWhatPerAddressWouldAllow(t *testing.T) {
	r := newRegistrar(t, []string{"127.0.0.1/32"}, 20, 3)

	for i, addr := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		if w := r.register(t, "127.0.0.1:9000", addr); w.Code != http.StatusCreated {
			t.Fatalf("registration %d should have been allowed, got %d", i, w.Code)
		}
	}
	w := r.register(t, "127.0.0.1:9000", "198.51.100.4")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a fresh address must not get past the instance bound, got %d", w.Code)
	}
	assertRefusal(t, w)
}

// The two bounds are one refusal. Telling a caller which of them it hit would tell it whether
// to spread across more addresses or to keep going because somebody else is paying.
func TestBothBoundsRefuseIdentically(t *testing.T) {
	perAddress := newRegistrar(t, nil, 1, 100)
	perAddress.register(t, "198.51.100.7:5555", "")
	first := perAddress.register(t, "198.51.100.7:5555", "")

	instance := newRegistrar(t, nil, 100, 1)
	instance.register(t, "198.51.100.7:5555", "")
	second := instance.register(t, "203.0.113.4:5555", "")

	if first.Code != second.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("the two refusals differ:\n  per-address %d %s\n  instance    %d %s",
			first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if got := first.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After would restore by arithmetic the distinction the body avoids, got %q", got)
	}
}

// A refused registration must not be a written row. Checking the bound after the insert would
// bound the response and leave the table growing at exactly the rate the bound exists to stop.
//
// The reaper counts them: every row here is a registration with no grant, so sweeping the lot
// answers how many the endpoint actually wrote.
func TestARefusedRegistrationWritesNothing(t *testing.T) {
	r := newRegistrar(t, nil, 1, 100)
	clientID(t, r.register(t, "198.51.100.7:5555", ""))

	for range 5 {
		if w := r.register(t, "198.51.100.7:5555", ""); w.Code != http.StatusTooManyRequests {
			t.Fatalf("everything past the first should have been refused, got %d", w.Code)
		}
	}

	r.server.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	written, err := r.server.SweepClients(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if written != 1 {
		t.Fatalf("the clients table holds %d rows, want only the one registration that was allowed", written)
	}
}

// Nothing configured is the same server as before: an unbounded endpoint rather than one
// that refuses by accident.
func TestNoLimitConfiguredIsUnbounded(t *testing.T) {
	r := newRegistrar(t, nil, 0, 0)
	if r.server.registrations != nil {
		t.Fatal("neither half configured should leave no limiter at all")
	}
	for i := range 50 {
		if w := r.register(t, "198.51.100.7:5555", ""); w.Code != http.StatusCreated {
			t.Fatalf("registration %d should have been allowed, got %d", i, w.Code)
		}
	}
}

// An IPv6 address is not a scarce thing. Counting them one at a time would be a per-address
// bound that anybody on IPv6 steps around by picking the next address in their own block.
func TestIPv6IsCountedByPrefix(t *testing.T) {
	if got := addressKey("2001:db8:1:2::1"); got != addressKey("2001:db8:1:2:ffff::9") {
		t.Fatalf("two addresses in one /64 must share a bucket, got %q", got)
	}
	if addressKey("2001:db8:1:2::1") == addressKey("2001:db8:1:3::1") {
		t.Fatal("addresses in different /64s must not share a bucket")
	}
	if got := addressKey("198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("an IPv4 address is counted whole, got %q", got)
	}
}

// Refusals must not push the window forward. A caller hammering the endpoint would otherwise
// lock itself out for longer than the configured rate says, which is a rate limit that no
// longer describes what it does.
func TestARefusalDoesNotExtendTheWindow(t *testing.T) {
	l := newRegistrationLimit(rate{count: 2, window: time.Hour}, rate{}, auth.TrustedProxies{})
	req := httptest.NewRequest(http.MethodPost, "/register", nil)
	req.RemoteAddr = "198.51.100.7:5555"

	for range 2 {
		if !l.allow(req) {
			t.Fatal("the first two should be allowed")
		}
	}
	for range 10 {
		if l.allow(req) {
			t.Fatal("everything past the count must be refused")
		}
	}
	if got := len(l.addresses[addressKey("198.51.100.7")].at); got != 2 {
		t.Fatalf("the bucket holds %d entries, want only the %d admissions", got, 2)
	}
}

// The other unauthenticated endpoint. It is deliberately not rate limited — see the comment
// at the handler — but it must not accept an unbounded body either.
func TestTokenBodyIsCapped(t *testing.T) {
	r := newRegistrar(t, nil, 20, 200)

	body := "grant_type=authorization_code&code=x&client_id=c&padding=" + strings.Repeat("a", 1<<17)
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized token request should be refused, got %d: %s", w.Code, w.Body.String())
	}
}

// The reaper. A registration that no grant references was never approved by anybody, and the
// rate limit above bounds only how fast they arrive.
func TestSweepClientsRemovesOnlyRegistrationsThatNeverBecameAGrant(t *testing.T) {
	ctx := context.Background()
	r := newRegistrar(t, nil, 20, 200)

	abandoned := clientID(t, r.register(t, "198.51.100.7:5555", ""))
	approved := clientID(t, r.register(t, "198.51.100.7:5555", ""))
	grantClient(t, r.db, approved)

	// A registration made just now is not stale however long the sweeper has been running,
	// so the clock moves rather than the rows.
	r.server.now = func() time.Time { return time.Now().Add(48 * time.Hour) }

	n, err := r.server.SweepClients(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d rows, want only the abandoned one", n)
	}
	if _, err := r.db.Client(ctx, abandoned); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("the abandoned registration should be gone, got %v", err)
	}
	if _, err := r.db.Client(ctx, approved); err != nil {
		t.Fatalf("a registration a grant names must survive: %v", err)
	}

	// And nothing is reclaimed before its time.
	fresh := clientID(t, r.register(t, "198.51.100.7:5555", ""))
	r.server.now = time.Now
	if n, err := r.server.SweepClients(ctx, 24*time.Hour); err != nil || n != 0 {
		t.Fatalf("a fresh registration must not be reclaimed: %d, %v", n, err)
	}
	if _, err := r.db.Client(ctx, fresh); err != nil {
		t.Fatalf("the fresh registration should still be there: %v", err)
	}
}

func TestSweepClientsIsOffWithoutATTL(t *testing.T) {
	r := newRegistrar(t, nil, 20, 200)
	id := clientID(t, r.register(t, "198.51.100.7:5555", ""))
	r.server.now = func() time.Time { return time.Now().Add(10 * 365 * 24 * time.Hour) }

	if n, err := r.server.SweepClients(context.Background(), 0); err != nil || n != 0 {
		t.Fatalf("a zero TTL must reclaim nothing: %d, %v", n, err)
	}
	if _, err := r.db.Client(context.Background(), id); err != nil {
		t.Fatalf("the registration should still be there: %v", err)
	}
}

func assertRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("a refusal must still be an OAuth error object: %v (%s)", err, w.Body.String())
	}
	if body.Error != "temporarily_unavailable" {
		t.Fatalf("want error=temporarily_unavailable, got %q", body.Error)
	}
	// The refusal names neither bound, and nothing in it distinguishes one from the other.
	for _, leak := range []string{"address", "instance", "limit", "rate", "per "} {
		if strings.Contains(strings.ToLower(body.Description), leak) {
			t.Fatalf("the refusal says %q, which tells a caller which bound it hit: %q", leak, body.Description)
		}
	}
}

// grantClient gives a registration a grant, which is what makes it something other than
// garbage.
func grantClient(t *testing.T, db *store.Store, clientID string) {
	t.Helper()
	ctx := context.Background()

	me, _, err := db.EnsureUser(ctx, user.User{
		Issuer: "https://idp.example.com", Subject: "ada", Email: "ada@example.com",
	}, store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		t.Fatal(err)
	}
	account := mail.Account{
		ID: "acct_1", Alias: "work", Address: "ada@example.com",
		Provider: mail.ProviderIMAP, Status: mail.StatusLinked,
	}
	if err := db.LinkAccount(ctx, me.ID, account, "sealed", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateGrant(ctx, &grant.Grant{
		ID: "grant_1", OwnerID: me.ID, ClientID: clientID, Label: "An agent",
		Accounts: []mail.AccountID{account.ID}, Caps: mail.NewSet(mail.CapRead),
	}); err != nil {
		t.Fatal(err)
	}
}
