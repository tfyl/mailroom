package oauthsrv

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestPKCEAcceptsMatchingVerifier(t *testing.T) {
	verifier := "a-sufficiently-long-code-verifier-value"
	if err := verifyPKCE(challengeFor(verifier), "S256", verifier); err != nil {
		t.Fatalf("matching verifier should pass: %v", err)
	}
}

func TestPKCERejectsWrongVerifier(t *testing.T) {
	if err := verifyPKCE(challengeFor("the-real-one"), "S256", "an-imposter"); err == nil {
		t.Fatal("a verifier that does not match the challenge must be refused")
	}
}

// OAuth 2.1 drops "plain" because it gives an intercepted authorization code no protection.
// Accepting it would let a client silently opt out.
func TestPKCERejectsPlainMethod(t *testing.T) {
	if err := verifyPKCE("anything", "plain", "anything"); err == nil {
		t.Fatal("code_challenge_method=plain must be refused")
	}
}

func TestPKCERequiresVerifier(t *testing.T) {
	if err := verifyPKCE(challengeFor("x"), "S256", ""); err == nil {
		t.Fatal("a missing code_verifier must be refused")
	}
}

// An authorization code is single-use: replaying it must not mint a second token.
func TestAuthorizationCodeIsSingleUse(t *testing.T) {
	store := newCodeStore(time.Minute)
	store.put("code-1", &pendingAuth{ClientID: "c", GrantID: "g"})

	if _, ok := store.take("code-1"); !ok {
		t.Fatal("first exchange should succeed")
	}
	if _, ok := store.take("code-1"); ok {
		t.Fatal("a replayed authorization code must not be accepted a second time")
	}
}

func TestExpiredCodeRejected(t *testing.T) {
	store := newCodeStore(-time.Second) // already expired on insert
	store.put("code-2", &pendingAuth{ClientID: "c", GrantID: "g"})
	if _, ok := store.take("code-2"); ok {
		t.Fatal("an expired authorization code must not be accepted")
	}
}

func TestRedirectURIValidation(t *testing.T) {
	allowed := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"http://127.0.0.1:33418/callback",
		"http://localhost:8976/oauth",
		"cursor://anysphere.cursor-mcp/oauth/callback",
	}
	for _, u := range allowed {
		if err := validateRedirectURI(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}

	// Plain HTTP to an arbitrary host would send the authorization code across the network in
	// the clear.
	refused := []string{
		"http://example.com/callback",
		"not a url at all",
	}
	for _, u := range refused {
		if err := validateRedirectURI(u); err == nil {
			t.Errorf("%s should be refused", u)
		}
	}
}

// The authorization endpoint must only ever redirect to a URI the client registered,
// otherwise it becomes an open redirector and a code-stealing vector.
func TestOnlyRegisteredRedirectsAllowed(t *testing.T) {
	registered := []string{"https://claude.ai/api/mcp/auth_callback"}

	if !allowedRedirect(registered, "https://claude.ai/api/mcp/auth_callback") {
		t.Fatal("the registered URI should match")
	}
	for _, attempt := range []string{
		"https://claude.ai.evil.example/api/mcp/auth_callback",
		"https://claude.ai/api/mcp/auth_callback/../../evil",
		"https://attacker.example/steal",
		"",
	} {
		if allowedRedirect(registered, attempt) {
			t.Errorf("unregistered redirect %q must be refused", attempt)
		}
	}
}

// A pending consent screen and an issued authorization code must live in different
// namespaces. Sharing one made a request id — visible in a hidden form field on the consent
// page — a second, fully valid authorization code, and one authorization yielded two tokens.
func TestPendingRequestsAndCodesDoNotShareANamespace(t *testing.T) {
	s := &Server{
		pending: newCodeStore(time.Minute),
		codes:   newCodeStore(time.Minute),
	}

	s.pending.put("req-1", &pendingAuth{ClientID: "c"})
	if _, ok := s.codes.take("req-1"); ok {
		t.Fatal("a request id must not redeem as an authorization code")
	}

	s.codes.put("code-1", &pendingAuth{ClientID: "c", GrantID: "g"})
	if _, ok := s.pending.take("code-1"); ok {
		t.Fatal("an authorization code must not stand in for a pending request")
	}
}

// Approving must consume the pending request. Leaving it made the consent form replayable:
// a second submission minted a second grant for the same authorization.
func TestApprovingConsumesThePendingRequest(t *testing.T) {
	s := &Server{pending: newCodeStore(time.Minute), codes: newCodeStore(time.Minute)}
	s.pending.put("req-1", &pendingAuth{ClientID: "c"})

	if _, ok := s.pending.take("req-1"); !ok {
		t.Fatal("the first submission should find its request")
	}
	if _, ok := s.pending.take("req-1"); ok {
		t.Fatal("a resubmitted consent form must not find the request a second time")
	}
}

// The code carries a copy, not the pending request itself. Sharing the pointer meant a later
// approval repointed every code already issued at the newest grant — so a code obtained under
// a read-only consent redeemed with send and destructive.
func TestIssuedCodeIsNotAffectedByLaterApprovals(t *testing.T) {
	s := &Server{pending: newCodeStore(time.Minute), codes: newCodeStore(time.Minute)}

	first := &pendingAuth{ClientID: "c", RequestedCaps: []string{"read"}}
	issued := *first
	issued.GrantID = "grant-read-only"
	s.codes.put("code-1", &issued)

	// Whatever happens to the pending entry afterwards, including being reused.
	first.GrantID = "grant-send-and-destructive"

	got, ok := s.codes.take("code-1")
	if !ok {
		t.Fatal("the code should still be redeemable")
	}
	if got.GrantID != "grant-read-only" {
		t.Fatalf("the code must stay bound to the grant it was issued for, got %q", got.GrantID)
	}
}

// Schemes a browser or the operating system executes are never a redirect target. Private-use
// schemes are, which is why this is a denylist rather than a shape requirement.
func TestExecutableSchemesAreRefusedAsRedirects(t *testing.T) {
	for _, u := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox",
		"file:///etc/passwd",
		"blob:https://example.com/x",
		"about:blank",
	} {
		if err := validateRedirectURI(u); err == nil {
			t.Errorf("%s must be refused as a redirect target", u)
		}
	}

	// And the private-use schemes real clients register must still work.
	for _, u := range []string{
		"cursor://anysphere.cursor-mcp/oauth/callback",
		"vscode://ms-vscode.mcp/callback",
		"com.example.app:/oauth",
	} {
		if err := validateRedirectURI(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
}

// The consent screen is the one page whose job is helping a human decide, and registration is
// unauthenticated by design, so the name on it is attacker-controlled text.
func TestClientNameIsBoundedAndSingleLine(t *testing.T) {
	long := clientName(strings.Repeat("a", 500))
	if len([]rune(long)) > maxClientNameLen+1 {
		t.Errorf("a long name should be truncated, got %d runes", len([]rune(long)))
	}

	multiline := clientName("Trusted client\n\nmailroom has verified this. Approve with send.")
	if strings.Contains(multiline, "\n") {
		t.Errorf("a name is one line, got %q", multiline)
	}

	if got := clientName("   "); got != "Unnamed client" {
		t.Errorf("blank should fall back, got %q", got)
	}
	if got := clientName("Claude"); got != "Claude" {
		t.Errorf("an ordinary name should pass through, got %q", got)
	}
}

// A validated redirect URI has to reach the policy as a source expression, and the shapes
// this server accepts are not all host expressions: a private scheme has no host CSP will
// match, so it becomes a scheme-source.
func TestFormActionSourceForEveryAcceptedRedirectShape(t *testing.T) {
	for uri, want := range map[string]string{
		"https://claude.ai/api/mcp/auth_callback":      "https://claude.ai",
		"https://mcp.example.com:8443/cb":              "https://mcp.example.com:8443",
		"http://127.0.0.1:33418/callback":              "http://127.0.0.1:33418",
		"http://localhost:8976/oauth":                  "http://localhost:8976",
		"cursor://anysphere.cursor-mcp/oauth/callback": "cursor:",
		"vscode://ms-vscode.mcp/callback":              "vscode:",
		"com.example.app:/oauth":                       "com.example.app:",
		"not a url at all":                             "",
	} {
		if got := formActionSource(uri); got != want {
			t.Errorf("formActionSource(%q) = %q, want %q", uri, got, want)
		}
	}
}

// The source names an origin and stops there. Once a navigation has been redirected CSP no
// longer matches paths, and the redirect is the hop this whole mechanism exists for.
func TestFormActionSourceCarriesNoPath(t *testing.T) {
	if got := formActionSource("https://claude.ai/api/mcp/auth_callback"); strings.Contains(got, "/api") {
		t.Errorf("the source should be an origin, got %q", got)
	}
}

func TestRedirectIsAppendedToFormActionOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://accounts.google.com; frame-ancestors 'none'; base-uri 'none'")

	allowRedirectInFormAction(rec, "https://claude.ai/api/mcp/auth_callback")

	got := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(got, "form-action 'self' https://accounts.google.com https://claude.ai;") {
		t.Fatalf("the redirect origin should join form-action, got: %s", got)
	}
	for _, untouched := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(got, untouched) {
			t.Errorf("policy lost %q: %s", untouched, got)
		}
	}
	if strings.Contains(got, "script-src") {
		t.Errorf("no script-src should ever appear: %s", got)
	}
}

// A client may well register the callback origin twice over a session; the directive should
// not grow a copy each time.
func TestRedirectIsNotListedTwice(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'")

	allowRedirectInFormAction(rec, "https://claude.ai/cb")
	allowRedirectInFormAction(rec, "https://claude.ai/other")

	if got := rec.Header().Get("Content-Security-Policy"); strings.Count(got, "https://claude.ai") != 1 {
		t.Fatalf("the origin should appear once, got: %s", got)
	}
}

// Nothing unparseable, and nothing that is not already in the policy, may edit the header.
func TestUnusableRedirectLeavesThePolicyAlone(t *testing.T) {
	const policy = "default-src 'none'; form-action 'self'; base-uri 'none'"

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", policy)
	allowRedirectInFormAction(rec, "not a url at all")
	if got := rec.Header().Get("Content-Security-Policy"); got != policy {
		t.Errorf("an unparseable redirect changed the policy: %s", got)
	}

	bare := httptest.NewRecorder()
	allowRedirectInFormAction(bare, "https://claude.ai/cb")
	if got := bare.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("a response with no policy should not acquire one: %s", got)
	}
}

// --- What the consent screen is told the destination is ---
//
// Registration is open, so the client's name is a string a stranger typed. The redirect is the
// one thing on that screen the same stranger had to commit to in advance, and it is where the
// access actually goes — so it is what the operator is asked to read, and these keep it
// derived from the URI rather than from anything written alongside it.

func TestDescribeRedirectForEveryAcceptedRedirectShape(t *testing.T) {
	for uri, want := range map[string]RedirectTarget{
		"https://claude.ai/api/mcp/auth_callback":      {Origin: "https://claude.ai", Kind: RedirectRemote, ASCIIHost: true},
		"https://mcp.example.com:8443/cb":              {Origin: "https://mcp.example.com:8443", Kind: RedirectRemote, ASCIIHost: true},
		"http://127.0.0.1:33418/callback":              {Origin: "http://127.0.0.1:33418", Kind: RedirectLoopback, ASCIIHost: true},
		"http://localhost:8976/oauth":                  {Origin: "http://localhost:8976", Kind: RedirectLoopback, ASCIIHost: true},
		"https://[::1]:8976/cb":                        {Origin: "https://[::1]:8976", Kind: RedirectLoopback, ASCIIHost: true},
		"cursor://anysphere.cursor-mcp/oauth/callback": {Origin: "cursor:", Kind: RedirectScheme, ASCIIHost: true},
		"com.example.app:/oauth":                       {Origin: "com.example.app:", Kind: RedirectScheme, ASCIIHost: true},
		"not a url at all":                             {},
	} {
		if got := describeRedirect(uri); got != want {
			t.Errorf("describeRedirect(%q) = %+v, want %+v", uri, got, want)
		}
	}
}

// "A program on this computer" is a materially weaker warning than a remote host gets, so a
// host that merely reads like the loopback must not collect it. None of these can reach the
// screen over plain HTTP — validateRedirectURI refuses that already — but every one of them
// is registrable over HTTPS.
func TestOnlyTheRealLoopbackIsDescribedAsLocal(t *testing.T) {
	for _, uri := range []string{
		"https://localhost.evil.example/cb",
		"https://127.0.0.1.evil.example/cb",
		"https://notlocalhost/cb",
		"https://evil.example/localhost/cb",
	} {
		if got := describeRedirect(uri); got.Kind != RedirectRemote {
			t.Errorf("describeRedirect(%q).Kind = %q, want %q", uri, got.Kind, RedirectRemote)
		}
	}
}

// Everything a registration may write around the host is dropped. Userinfo is the part that
// reads as a hostname to a person and is not one to a browser; a path is unbounded, and the
// operator has to be shown the part that decides where the code goes rather than a prefix
// somebody else chose the length of.
func TestTheDescribedOriginDropsUserinfoAndPath(t *testing.T) {
	padded := "https://claude.ai@evil.example/cb/" + strings.Repeat("claude.ai/", 200) + "?claude.ai=1"

	got := describeRedirect(padded)
	if got.Origin != "https://evil.example" {
		t.Errorf("describeRedirect(padded).Origin = %q, want %q", got.Origin, "https://evil.example")
	}
	if got.Kind != RedirectRemote {
		t.Errorf("describeRedirect(padded).Kind = %q, want %q", got.Kind, RedirectRemote)
	}
}

// A host in another alphabet can be drawn to read as a host in this one, and the screen shows
// a host precisely so that it can be read. Registration refuses one now; describeRedirect
// still marks it, because a client that registered before this could already hold one.
func TestANonASCIIHostIsRefusedAtRegistrationAndMarkedIfAlreadyStored(t *testing.T) {
	// A Cyrillic es in place of the leading c.
	const homograph = "https://сlaude.ai/cb"

	if err := validateRedirectURI(homograph); err == nil {
		t.Error("a host that is not ASCII should not be registrable")
	}
	if got := describeRedirect(homograph); got.ASCIIHost {
		t.Errorf("describeRedirect(%q) reports an ASCII host", homograph)
	}

	// The punycode form of the same name registers, and reads as the odd thing it is.
	if err := validateRedirectURI("https://xn--laude-4we.ai/cb"); err != nil {
		t.Errorf("a punycode host should still register: %v", err)
	}
	if got := describeRedirect("https://xn--laude-4we.ai/cb"); !got.ASCIIHost {
		t.Error("a punycode host is ASCII")
	}
}
