// Package preflight checks a deployment against reality before somebody discovers the
// problem by failing to sign in.
//
// It exists because of one specific failure. Google matches OAuth redirect URIs exactly, and
// mailroom uses two of them on different paths — one to link a mailbox, one to sign in. An
// operator who registers only the first has a configuration that looks entirely correct,
// starts cleanly, links mailboxes perfectly, and cannot be logged into. Nothing on the server
// can tell, because the refusal happens at Google.
//
// So these checks ask Google, rather than reading configuration back to the operator.
package preflight

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Status is how a check came out. Unknown is a first-class outcome: several of these depend
// on undocumented behaviour or on a third party being reachable, and reporting a guess as a
// pass would defeat the point of checking at all.
type Status string

const (
	OK      Status = "ok"
	Problem Status = "problem"
	Unknown Status = "unknown"
	// Skipped is a check that did not apply — a provider with no client configured, or one
	// whose answer this deployment cannot obtain. Distinct from Unknown, which means the
	// check ran and the answer could not be read.
	Skipped Status = "skipped"
)

// Result is one check.
type Result struct {
	Name   string
	Status Status
	Detail string
	// Fix is what to do about it, empty when there is nothing to do.
	Fix string
}

// CheckRedirectURI asks Google whether a redirect URI is registered on a client.
//
// The authorization endpoint answers a well-formed request with a redirect: to the sign-in
// page when the URI is registered, and to /signin/oauth/error when it is not. The error page
// carries an authError parameter, base64 of a payload whose first field is the error code
// string — redirect_uri_mismatch for an unregistered URI, invalid_request for one Google will
// not accept at all, which distinguishes "you forgot to add this" from "this can never work".
//
// None of that shape is documented, so an unrecognised answer is reported as Unknown rather
// than assumed to be either outcome. The request is a GET that stops at the first redirect:
// it authenticates nobody, consents to nothing, and grants nothing.
func CheckRedirectURI(ctx context.Context, client *http.Client, clientID, redirectURI string) (Status, string) {
	if clientID == "" {
		return Unknown, "no Google client id is configured"
	}

	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"openid"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), nil)
	if err != nil {
		return Unknown, err.Error()
	}

	// The answer is the redirect itself, so following it would throw away the result and
	// land on a sign-in page. Enforced on a copy here rather than trusting the caller to
	// have configured it: a plain http.Client follows redirects, and the failure mode is
	// every check silently returning Unknown.
	stop := *client
	stop.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := stop.Do(req)
	if err != nil {
		return Unknown, "could not reach Google: " + err.Error()
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if location == "" {
		return Unknown, fmt.Sprintf("Google answered %d without a redirect", resp.StatusCode)
	}

	code, err := authError(location)
	switch {
	case errors.Is(err, errNotAnError):
		return OK, "registered"
	case err != nil:
		return Unknown, err.Error()
	case code == "redirect_uri_mismatch":
		return Problem, "not registered on this client"
	case code == "invalid_client":
		// Distinct from a URI problem, and at least as common: the client id is wrong or
		// belongs to a project that no longer has it. Saying "Google refused the URI" here
		// would send somebody to check a URL that is perfectly fine.
		return Problem, "Google does not recognise this client id"
	default:
		return Problem, "Google refused it: " + code
	}
}

var errNotAnError = errors.New("the response is not an error page")

// authError pulls the error code out of the redirect Google answers with.
func authError(location string) (string, error) {
	u, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("could not read Google's response: %w", err)
	}
	if !strings.Contains(u.Path, "/signin/oauth/error") {
		return "", errNotAnError
	}

	raw := u.Query().Get("authError")
	if raw == "" {
		return "", errors.New("Google refused the request without saying why")
	}
	// Padding is stripped, and the payload is a length-prefixed protobuf field rather than
	// text, so only the leading code is worth reading.
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	if err != nil {
		return "", fmt.Errorf("Google's error was not readable: %w", err)
	}
	for _, known := range []string{"redirect_uri_mismatch", "invalid_request", "invalid_client", "access_denied"} {
		if strings.Contains(string(decoded), known) {
			return known, nil
		}
	}
	return "", errors.New("Google refused the request for a reason this check does not recognise")
}

// GoogleRedirects checks both URIs a mailroom instance uses, given its public URL.
//
// login is false on an instance that does not offer Google sign-in, where the second URI is
// genuinely not needed and reporting it missing would be noise.
func GoogleRedirects(ctx context.Context, publicURL, clientID string, login bool) []Result {
	client := &http.Client{Timeout: 15 * time.Second}
	base := strings.TrimSuffix(publicURL, "/")

	checks := []struct {
		name, path, fix string
	}{{
		name: "Google redirect URI for linking a mailbox",
		path: "/accounts/link/google/callback",
		fix:  "Add it under Authorized redirect URIs on the OAuth client, keeping any already there.",
	}}
	if login {
		checks = append(checks, struct{ name, path, fix string }{
			name: "Google redirect URI for signing in",
			path: "/auth/google/callback",
			fix: "Add it under Authorized redirect URIs on the OAuth client. It is a different " +
				"path from the linking one and Google matches exactly, so registering only " +
				"the other leaves an instance that links mailboxes and cannot be logged into.",
		})
	}

	var out []Result
	for _, c := range checks {
		status, detail := CheckRedirectURI(ctx, client, clientID, base+c.path)
		r := Result{Name: c.name, Status: status, Detail: base + c.path + " — " + detail}
		if status == Problem {
			r.Fix = c.fix
		}
		out = append(out, r)
	}
	return out
}

// Report renders results for a terminal and says whether anything is definitely wrong.
func Report(results []Result) (string, bool) {
	var b strings.Builder
	problems := false
	for _, r := range results {
		mark := map[Status]string{OK: "ok  ", Problem: "FAIL", Unknown: "?   ", Skipped: "--  "}[r.Status]
		fmt.Fprintf(&b, "%s  %s\n      %s\n", mark, r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(&b, "      → %s\n", r.Fix)
		}
		if r.Status == Problem {
			problems = true
		}
	}
	return b.String(), problems
}
