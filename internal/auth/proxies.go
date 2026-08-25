package auth

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the set of source addresses permitted to speak for somebody else.
//
// There is one of these and there must only ever be one. Two places in this server ask a
// question they cannot answer from the connection alone — forward-auth asks who the operator
// is, and the registration bound asks which client an unauthenticated request came from — and
// both answers arrive in a header any caller can write. A second, differently-shaped notion of
// which callers may assert such a header would be a second thing to configure, a second thing
// to get wrong, and a deployment where one of them is right.
//
// Empty is the safe value and is what an unconfigured instance has: nothing forwarded is
// believed from anybody, so every request is attributed to the address that opened the
// connection. That is wrong behind a proxy — every caller then looks like the proxy — but it
// is wrong in the direction that over-counts rather than the one that lets a stranger pick
// their own identity. See docs/deploying.md.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies reads the configured list. An entry is a CIDR, or a bare address
// meaning that host alone.
func ParseTrustedProxies(entries []string) (TrustedProxies, error) {
	var t TrustedProxies
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			// A bare address is a /32 or /128.
			if ip := net.ParseIP(entry); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				t.nets = append(t.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
		}
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return TrustedProxies{}, fmt.Errorf("trusted proxy %q is not an IP or CIDR: %w", entry, err)
		}
		t.nets = append(t.nets, n)
	}
	return t, nil
}

// Empty reports whether anything is trusted at all.
func (t TrustedProxies) Empty() bool { return len(t.nets) == 0 }

// Contains reports whether an address is one of the configured proxies.
func (t TrustedProxies) Contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustsRequest reports whether a request arrived from a source on the list.
func (t TrustedProxies) TrustsRequest(r *http.Request) bool {
	return t.Contains(remoteIP(r))
}

// ClientIP is the address a request should be attributed to.
//
// The connection's own address unless it belongs to a configured proxy, in which case the
// rightmost address in X-Forwarded-For that is not itself a configured proxy — the last hop
// before the request entered infrastructure this deployment controls. Reading it from the
// right is what makes it unspoofable: a proxy appends the address it is talking to, so
// anything a caller wrote itself ends up to the left of the address the proxy observed, and
// is never reached.
//
// It returns the empty string when the source address cannot be parsed at all, which callers
// must treat as "unattributable" rather than as a key. A single bucket shared by everything
// unparseable would be a bucket a caller could deliberately land in.
func (t TrustedProxies) ClientIP(r *http.Request) string {
	remote := remoteIP(r)
	if remote == nil {
		return ""
	}
	if !t.Contains(remote) {
		// Includes every source when nothing is configured. An untrusted caller's header is
		// not read at all, so it cannot choose which bucket it falls in.
		return remote.String()
	}
	for _, hop := range forwardedFor(r) {
		ip := net.ParseIP(hop)
		if ip == nil {
			// Attacker-controlled text where an address should be. Everything further left
			// was written by the same hand, so the chain stops being evidence here and the
			// proxy's own address is the last thing known to be true.
			break
		}
		if t.Contains(ip) {
			continue
		}
		return ip.String()
	}
	return remote.String()
}

// forwardedFor returns the X-Forwarded-For chain, rightmost first.
//
// Every header instance is joined before splitting, because a chain that passed through two
// proxies may arrive as two headers and the order across them is the order they were added.
func forwardedFor(r *http.Request) []string {
	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(header, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	return hops
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}
