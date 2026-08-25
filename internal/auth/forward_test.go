package auth

import (
	"errors"
	"net/http"
	"testing"
)

func request(remoteAddr string, headers map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/accounts", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// Refusing to start without a trusted-proxy list is the whole safety property of this mode.
func TestForwardRefusesEmptyTrustedProxies(t *testing.T) {
	if _, err := NewForward("X-Forwarded-Email", nil, ""); err == nil {
		t.Fatal("forward-auth must not start without a trusted proxy list")
	}
}

// The header is trivially forgeable, so it must be ignored from any untrusted source.
func TestForwardIgnoresHeaderFromUntrustedSource(t *testing.T) {
	f, err := NewForward("X-Forwarded-Email", []string{"10.0.0.0/8"}, "")
	if err != nil {
		t.Fatal(err)
	}

	spoofed := request("203.0.113.9:5555", map[string]string{"X-Forwarded-Email": "admin@example.com"})
	if _, err := f.Identify(spoofed); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a forged header from outside the trusted range must not authenticate, got %v", err)
	}
}

func TestForwardAcceptsHeaderFromTrustedProxy(t *testing.T) {
	f, err := NewForward("X-Forwarded-Email", []string{"10.0.0.0/8"}, "")
	if err != nil {
		t.Fatal(err)
	}

	op, err := f.Identify(request("10.1.2.3:4444", map[string]string{"X-Forwarded-Email": "you@example.com"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op.Email != "you@example.com" {
		t.Fatalf("want the forwarded identity, got %q", op.Email)
	}
}

func TestForwardBareIPIsTreatedAsSingleHost(t *testing.T) {
	f, err := NewForward("X-Forwarded-Email", []string{"192.0.2.10"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Identify(request("192.0.2.10:1", map[string]string{"X-Forwarded-Email": "a@example.com"})); err != nil {
		t.Fatalf("the named host should be trusted: %v", err)
	}
	if _, err := f.Identify(request("192.0.2.11:1", map[string]string{"X-Forwarded-Email": "a@example.com"})); err == nil {
		t.Fatal("a neighbouring address must not be trusted")
	}
}

func TestForwardRequiredGroup(t *testing.T) {
	f, err := NewForward("X-Forwarded-Email", []string{"10.0.0.0/8"}, "mailroom-admins")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Authorize(Operator{Groups: []string{"everyone"}}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("an operator outside the required group must be refused, got %v", err)
	}
	if err := f.Authorize(Operator{Groups: []string{"everyone", "mailroom-admins"}}); err != nil {
		t.Fatalf("an operator in the required group should be allowed: %v", err)
	}
}

// Missing header from a trusted proxy is still no session — the proxy did not authenticate
// anybody, and an empty identity must never become a valid operator.
func TestForwardEmptyHeaderIsNotAnIdentity(t *testing.T) {
	f, _ := NewForward("X-Forwarded-Email", []string{"10.0.0.0/8"}, "")
	if _, err := f.Identify(request("10.0.0.1:1", nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}
