package auth

import "testing"

// A sign-in that returns you to where you started signing in loops forever. The login page
// links directly to /auth/<id>/start, so this is the ordinary path rather than an edge case.
func TestSafeNextRefusesLoginPaths(t *testing.T) {
	for _, in := range []string{"/auth/google/start", "/auth/callback", "/auth/dexa/callback", "/login"} {
		if got := safeNext(in); got == in {
			t.Errorf("safeNext(%q) returned it unchanged; signing in would loop", in)
		}
	}
}

// An open redirect hanging off a completed sign-in is a phishing gift.
func TestSafeNextRefusesOffsiteRedirects(t *testing.T) {
	for _, in := range []string{"https://evil.example/", "//evil.example/", "http://evil.example", ""} {
		if got := safeNext(in); got != "/accounts" {
			t.Errorf("safeNext(%q) = %q; want the local fallback", in, got)
		}
	}
}

func TestSafeNextKeepsOrdinaryPaths(t *testing.T) {
	for _, in := range []string{"/accounts", "/grants", "/audit"} {
		if got := safeNext(in); got != in {
			t.Errorf("safeNext(%q) = %q; an ordinary destination should survive", in, got)
		}
	}
}

// Every one of these resolves to somewhere off this instance in a browser, and the ones with
// a backslash do so only because the WHATWG URL parser reads it as a separator — which is
// why the rule is what a destination may be rather than a list of the shapes seen so far.
func TestSafeNextRefusesAnythingCarryingAnAuthority(t *testing.T) {
	for _, in := range []string{
		"//evil.com",
		`/\evil.com`,
		`\\evil.com`,
		"https://evil.com",
		"/%5Cevil.com",
		"//evil.com/path",
		"///evil.com",
		"http://evil.example",
		"//user@evil.com/",
		"javascript:alert(1)",
		"",
	} {
		if got := safeNext(in); got != "/accounts" {
			t.Errorf("safeNext(%q) = %q; want the local fallback", in, got)
		}
	}
}

func TestSafeNextKeepsAPathAndItsQuery(t *testing.T) {
	for in, want := range map[string]string{
		"/accounts":   "/accounts",
		"/grants?x=1": "/grants?x=1",
		"/audit":      "/audit",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q; want %q", in, got, want)
		}
	}
}
