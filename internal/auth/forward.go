package auth

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Forward trusts an identity header set by a reverse proxy that has already authenticated
// the operator — Cloudflare Access, oauth2-proxy, Authelia, Tailscale, Pomerium.
//
// This mode is a loaded gun. The header is trivially forgeable by anyone who can reach the
// port directly, so it is only ever read from a source address inside the configured trusted
// set, and construction fails outright when that set is empty. Bind to a private interface
// as well: the CIDR check is a second line of defence, not the only one.
type Forward struct {
	header  string
	trusted []*net.IPNet
	group   string
}

func NewForward(header string, trustedProxies []string, requiredGroup string) (*Forward, error) {
	if header == "" {
		return nil, fmt.Errorf("forward-auth requires a header name")
	}
	if len(trustedProxies) == 0 {
		return nil, fmt.Errorf("forward-auth requires at least one trusted proxy: without one " +
			"the identity header would be accepted from any source")
	}

	var nets []*net.IPNet
	for _, entry := range trustedProxies {
		entry = strings.TrimSpace(entry)
		if !strings.Contains(entry, "/") {
			// A bare address is a /32 or /128.
			if ip := net.ParseIP(entry); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
		}
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not an IP or CIDR: %w", entry, err)
		}
		nets = append(nets, n)
	}
	return &Forward{header: header, trusted: nets, group: requiredGroup}, nil
}

func (f *Forward) Mode() string { return "forward" }

func (f *Forward) Identify(r *http.Request) (Operator, error) {
	if !f.trustedSource(r) {
		// Deliberately reported as "no session" rather than "your address is not trusted":
		// an untrusted caller should not learn that a header-based bypass exists at all.
		return Operator{}, ErrNoSession
	}
	email := strings.TrimSpace(r.Header.Get(f.header))
	if email == "" {
		return Operator{}, ErrNoSession
	}
	// The proxy is the issuer here. Naming it distinguishes these identities from OIDC ones
	// carrying the same email, which would otherwise collide in the user table.
	op := Operator{Issuer: "forward-auth", Subject: email, Email: email, Name: email}
	if groups := r.Header.Get("X-Forwarded-Groups"); groups != "" {
		op.Groups = splitAndTrim(groups)
	}
	return op, nil
}

func (f *Forward) Authorize(op Operator) error {
	if f.group == "" {
		return nil
	}
	for _, g := range op.Groups {
		if g == f.group {
			return nil
		}
	}
	return ErrNotAuthorized
}

// StartLogin reports false: the proxy in front owns the login, and rendering a form here
// would offer a path that could never succeed.
func (f *Forward) StartLogin(http.ResponseWriter, *http.Request) bool { return false }

// Logout is a no-op; the session belongs to the proxy.
func (f *Forward) Logout(http.ResponseWriter, *http.Request) {}

func (f *Forward) trustedSource(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range f.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
