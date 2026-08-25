package preflight

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// stub answers every request with one canned redirect, which is all these checks read.
type stub struct {
	location string
	status   int
}

func (s stub) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	if s.location != "" {
		h.Set("Location", s.location)
	}
	code := s.status
	if code == 0 {
		code = http.StatusFound
	}
	return &http.Response{StatusCode: code, Header: h, Body: http.NoBody}, nil
}

// The Location values below have the shape Google's live authorization endpoint returned on
// 18 August 2026, against a real client with one of the two URIs registered. The shape is
// what these tests depend on, so it is kept exactly; the identifiers inside it are not, and
// have been replaced with zeroes.
//
// The first version of this file kept the captured strings whole, which put a real project
// number and a live session parameter in the repository. Neither is a credential — a project
// number is visible inside every OAuth client id, and those are public by design — but this
// repository's own rule is that no real identifier goes in, and a rule with an exception
// nobody wrote down is not a rule.
const (
	liveRegistered = "https://accounts.google.com/v3/signin/identifier?opparams=%253F&dsh=S0000000000%3A0000000000000000&client_id=000000000000-x.apps.googleusercontent.com"
	liveMismatch   = "https://accounts.google.com/signin/oauth/error?authError=ChVyZWRpcmVjdF91cmlfbWlzbWF0Y2gSsAEKWW91IGNhbid0IHNpZ24gaW4gdG8gdGhpcyBhcHAgYmVjYXVzZSBpdCBk" // gitleaks:allow — base64 of Google's error payload, not a credential
	liveInvalid    = "https://accounts.google.com/signin/oauth/error?authError=Cg9pbnZhbGlkX3JlcXVlc3QS3gEKWW91IGNhbid0IHNpZ24gaW4gdG8gdGhpcyBhcHAgYmVjYXVzZSBpdCBkb2Vzbid"  // gitleaks:allow — base64 of Google's error payload, not a credential
)

// Built rather than captured: the shape is the same length-prefixed field as the two above,
// and constructing it keeps a real client id out of the repository.
var liveUnknownClient = func() string {
	body := "invalid_client"
	payload := base64.RawURLEncoding.EncodeToString(append([]byte{0x0a, byte(len(body))}, body...))
	return "https://accounts.google.com/signin/oauth/error?authError=" + payload
}()

func TestCheckRedirectURIAgainstGooglesRealAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		location string
		want     Status
		detail   string
	}{
		{"registered", liveRegistered, OK, "registered"},
		{"not registered", liveMismatch, Problem, "not registered"},
		{"google will not accept it at all", liveInvalid, Problem, "invalid_request"},
		// A wrong client id needs its own answer: reported as a refused URI, it sends
		// somebody to check a URL that is perfectly correct.
		{"the client id is wrong", liveUnknownClient, Problem, "does not recognise this client id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: stub{location: tc.location}}
			status, detail := CheckRedirectURI(context.Background(), client, "client-id", "https://mail.example.com/x")
			if status != tc.want {
				t.Fatalf("want %s, got %s (%s)", tc.want, status, detail)
			}
			if !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail should mention %q, got %q", tc.detail, detail)
			}
		})
	}
}

// An answer this check does not recognise must not be reported as a pass. The response shape
// is undocumented, so it is exactly the thing that can change without warning, and a silent
// pass would turn a broken deployment into a green check.
func TestUnrecognisedAnswersAreUnknownRatherThanOK(t *testing.T) {
	for _, tc := range []struct{ name, location string }{
		{"no redirect at all", ""},
		{"an error page with no code", "https://accounts.google.com/signin/oauth/error"},
		{"an error code this check has never seen", "https://accounts.google.com/signin/oauth/error?authError=" + "Cgxzb21ldGhpbmdfbmV3"},
		{"something unparseable", "https://accounts.google.com/signin/oauth/error?authError=!!!not-base64!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: stub{location: tc.location}}
			if status, detail := CheckRedirectURI(context.Background(), client, "id", "https://x/y"); status != Unknown {
				t.Fatalf("want unknown, got %s (%s)", status, detail)
			}
		})
	}
}

func TestNoClientIDIsUnknownNotAFailure(t *testing.T) {
	client := &http.Client{Transport: stub{location: liveMismatch}}
	if status, _ := CheckRedirectURI(context.Background(), client, "", "https://x/y"); status != Unknown {
		t.Fatalf("an instance with no Google client cannot fail this check, got %s", status)
	}
}

// An instance that does not offer Google sign-in genuinely does not need the second URI, and
// reporting it missing would train people to ignore the output.
func TestLoginURIIsOnlyCheckedWhenGoogleSignInIsConfigured(t *testing.T) {
	ctx := context.Background()
	if got := len(GoogleRedirects(ctx, "https://mail.example.com", "", false)); got != 1 {
		t.Fatalf("want only the linking URI checked, got %d checks", got)
	}
	if got := len(GoogleRedirects(ctx, "https://mail.example.com", "", true)); got != 2 {
		t.Fatalf("want both URIs checked, got %d checks", got)
	}
}

func TestReportFlagsProblemsAndNotUnknowns(t *testing.T) {
	_, problems := Report([]Result{{Name: "a", Status: Unknown}, {Name: "b", Status: OK}})
	if problems {
		t.Fatal("an unknown is not a failure: it means the check could not tell")
	}
	out, problems := Report([]Result{{Name: "a", Status: Problem, Detail: "d", Fix: "do the thing"}})
	if !problems {
		t.Fatal("a problem must be reported as one")
	}
	if !strings.Contains(out, "do the thing") {
		t.Fatalf("the fix should be printed, got: %s", out)
	}
}
