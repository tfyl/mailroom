package auth

import (
	"net/http"
	"testing"
)

func proxied(remoteAddr string, forwarded ...string) *http.Request {
	r := request(remoteAddr, nil)
	for _, v := range forwarded {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// The whole point of the trust boundary: a caller that is not a configured proxy may assert
// whatever it likes and be attributed to itself anyway.
func TestClientIPIgnoresAForgedHeaderFromAnUntrustedSource(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	for _, forged := range []string{"1.2.3.4", "10.0.0.9", "203.0.113.9, 10.0.0.9", "nonsense"} {
		got := trusted.ClientIP(proxied("203.0.113.9:5555", forged))
		if got != "203.0.113.9" {
			t.Errorf("X-Forwarded-For %q from an untrusted source gave %q, want the connection's own address", forged, got)
		}
	}
}

// Unset must not mean "believe everybody". An instance that has configured nothing attributes
// every request to the address that opened the connection.
func TestClientIPTrustsNothingWhenUnset(t *testing.T) {
	var none TrustedProxies
	if !none.Empty() {
		t.Fatal("an unconfigured list should be empty")
	}
	if got := none.ClientIP(proxied("127.0.0.1:9000", "1.2.3.4")); got != "127.0.0.1" {
		t.Fatalf("with nothing trusted the header must be ignored, got %q", got)
	}
}

func TestClientIPReadsTheChainFromTheRight(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1/32", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		forwarded []string
		want      string
	}{
		// The ordinary case: one hop, and the address it observed.
		{"one hop", []string{"198.51.100.7"}, "198.51.100.7"},
		// A caller that wrote its own entry before the proxy appended what it saw. The
		// forged half is to the left and is never reached.
		{"forged prefix", []string{"192.0.2.1, 198.51.100.7"}, "198.51.100.7"},
		// A caller claiming to be a trusted proxy does not become one by saying so.
		{"forged trusted prefix", []string{"10.0.0.5, 198.51.100.7"}, "198.51.100.7"},
		// Two proxies in front, both configured: the client is the last address that is
		// neither of them.
		{"two hops", []string{"198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		// Split across two header instances, which is how a two-proxy chain often arrives.
		{"two headers", []string{"198.51.100.7", "10.0.0.5"}, "198.51.100.7"},
		// Nothing forwarded at all: the proxy itself is all there is.
		{"no header", nil, "127.0.0.1"},
		// Text where an address should be. Everything left of it was written by the same
		// hand, so the chain stops being evidence and the proxy's own address stands.
		{"unparseable", []string{"192.0.2.1, not-an-ip"}, "127.0.0.1"},
		// Every hop is a configured proxy, so no client address was ever recorded.
		{"all trusted", []string{"10.0.0.5, 10.0.0.6"}, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trusted.ClientIP(proxied("127.0.0.1:9000", tc.forwarded...)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxiesRefusesNonsense(t *testing.T) {
	if _, err := ParseTrustedProxies([]string{"10.0.0.0/8", "not-a-network"}); err == nil {
		t.Fatal("an entry that is neither an IP nor a CIDR must be a configuration error")
	}
}

func TestParseTrustedProxiesTreatsABareAddressAsOneHost(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"192.0.2.10", "  ", "2001:db8::1"})
	if err != nil {
		t.Fatal(err)
	}
	if !trusted.TrustsRequest(request("192.0.2.10:1", nil)) {
		t.Fatal("the named host should be trusted")
	}
	if trusted.TrustsRequest(request("192.0.2.11:1", nil)) {
		t.Fatal("a neighbouring address must not be trusted")
	}
	if !trusted.TrustsRequest(request("[2001:db8::1]:1", nil)) {
		t.Fatal("the named IPv6 host should be trusted")
	}
	if trusted.TrustsRequest(request("[2001:db8::2]:1", nil)) {
		t.Fatal("a neighbouring IPv6 address must not be trusted")
	}
}
