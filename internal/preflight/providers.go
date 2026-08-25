package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Deployment is the configuration these checks read. It is a copy of the handful of fields
// that matter rather than the config type itself, so this package stays a leaf and can be
// tested without building a whole configuration.
type Deployment struct {
	PublicURL string

	GoogleClientID string
	// GoogleSignIn is whether operators sign in with Google, which decides whether the second
	// redirect URI is required.
	GoogleSignIn bool

	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftTenant       string

	ZohoClientID string
	ZohoRegion   string
}

// All runs every check that can be made without a linked mailbox.
//
// The providers differ enormously in how much of this is possible, and the report says so
// rather than staying quiet about it. Google validates a redirect URI at its authorization
// endpoint, so an unregistered one is caught here. Microsoft and Zoho do not: both answer a
// registered and an unregistered URI with the identical sign-in page, and only refuse after
// somebody has signed in. A check written against their documentation would pass every time,
// which is worse than no check at all — so those are reported as unverifiable, with the URI
// to compare by eye.
func All(ctx context.Context, d Deployment) []Result {
	client := &http.Client{Timeout: 15 * time.Second}

	out := []Result{publicURL(d.PublicURL)}
	out = append(out, google(ctx, d)...)
	out = append(out, microsoft(ctx, client, d)...)
	out = append(out, zoho(ctx, client, d)...)
	out = append(out, imap())
	return out
}

// publicURL checks the one value every provider derives its redirect URI from.
//
// A trailing slash is deliberately not among the checks: config.Load trims it before anything
// reads it, so it causes no problem and reporting one would send an operator to fix something
// that already works. A path and a non-https scheme are different — config accepts both, and
// every provider then refuses a redirect URI that looks correct in the configuration file.
func publicURL(raw string) Result {
	const name = "Public URL"
	if raw == "" {
		return Result{Name: name, Status: Problem, Detail: "MAILROOM_PUBLIC_URL is not set",
			Fix: "Set it to the URL this instance is reached at, such as https://mail.example.com."}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Result{Name: name, Status: Problem, Detail: raw + " — " + err.Error()}
	}
	switch {
	case u.Path != "" && u.Path != "/":
		return Result{Name: name, Status: Problem, Detail: raw + " — has a path",
			Fix: "Every redirect URI is built by appending to this, so a path here produces URIs no provider has registered."}
	case u.Scheme != "https" && u.Hostname() != "localhost":
		return Result{Name: name, Status: Problem, Detail: raw + " — is not https",
			Fix: "Google, Microsoft and Zoho all refuse a non-https redirect URI for anything but localhost."}
	}
	return Result{Name: name, Status: OK, Detail: raw}
}

func google(ctx context.Context, d Deployment) []Result {
	if d.GoogleClientID == "" {
		return []Result{{Name: "Google", Status: Skipped,
			Detail: "no client configured — linking a Gmail mailbox is switched off"}}
	}
	return GoogleRedirects(ctx, d.PublicURL, d.GoogleClientID, d.GoogleSignIn)
}

// guid is the shape Entra requires of an application id. Checked locally because it is the
// one Microsoft mistake the authorization endpoint does catch — AADSTS700038 for an
// identifier that is not a GUID — and doing it here costs no request.
var guid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// multiTenantSegments are the three tenant values that name a class of directory rather than
// one directory. The client-credentials check below cannot run against any of them.
var multiTenantSegments = map[string]bool{"common": true, "organizations": true, "consumers": true}

func microsoft(ctx context.Context, client *http.Client, d Deployment) []Result {
	if d.MicrosoftClientID == "" {
		return []Result{{Name: "Microsoft", Status: Skipped,
			Detail: "no client configured — linking an Outlook or Microsoft 365 mailbox is switched off"}}
	}

	tenant := d.MicrosoftTenant
	if tenant == "" {
		tenant = "common"
	}

	out := []Result{}

	if guid.MatchString(d.MicrosoftClientID) {
		out = append(out, Result{Name: "Microsoft client id", Status: OK, Detail: "is a well-formed application id"})
	} else {
		out = append(out, Result{Name: "Microsoft client id", Status: Problem,
			Detail: d.MicrosoftClientID + " is not a GUID",
			Fix: "Entra refuses this with AADSTS700038 before it looks at anything else. Copy the " +
				"Application (client) ID from the registration's overview — not the Object ID and " +
				"not the Secret ID, both of which are also GUIDs and sit nearby."})
	}

	out = append(out, microsoftTenant(ctx, client, tenant))

	// Only possible against one directory. Microsoft answers client credentials at /common
	// with AADSTS53003 rather than a useful refusal, so on the default tenant there is no way
	// to tell a good secret from a bad one without a mailbox.
	if multiTenantSegments[strings.ToLower(tenant)] {
		out = append(out, Result{Name: "Microsoft client secret", Status: Skipped,
			Detail: "cannot be checked with MAILROOM_MICROSOFT_TENANT=" + tenant +
				" — a client-credentials request against a tenant naming a class of directory is refused for reasons unrelated to the secret"})
	} else if d.MicrosoftClientSecret == "" {
		out = append(out, Result{Name: "Microsoft client secret", Status: Problem,
			Detail: "MAILROOM_MICROSOFT_CLIENT_SECRET is empty while a client id is set",
			Fix:    "Linking fails at the code exchange, after the person consenting has already approved it."})
	} else {
		out = append(out, microsoftSecret(ctx, client, tenant, d.MicrosoftClientID, d.MicrosoftClientSecret))
	}

	// Reported, not checked, and the detail says which. Verified against Entra on
	// 22 August 2026: an authorization request naming an unregistered redirect URI returns
	// the same 40KB sign-in page as a registered one, because the URI is compared only after
	// the person signs in. There is no pre-authentication answer to read.
	out = append(out, Result{Name: "Microsoft redirect URI", Status: Unknown,
		Detail: strings.TrimSuffix(d.PublicURL, "/") + "/accounts/link/microsoft/callback" +
			" — Entra answers a registered and an unregistered URI identically before sign-in, so this cannot be verified from here",
		Fix: "Compare it by eye against Authentication → Redirect URIs on the registration. " +
			"If it is missing, consent succeeds and the redirect back fails with AADSTS50011."})

	return out
}

// microsoftTenant asks the identity platform whether the configured tenant exists. It catches
// a typo in MAILROOM_MICROSOFT_TENANT, which otherwise fails at consent for the operator
// rather than at startup.
func microsoftTenant(ctx context.Context, client *http.Client, tenant string) Result {
	const name = "Microsoft tenant"
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/v2.0/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{Name: name, Status: Unknown, Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Name: name, Status: Unknown, Detail: "could not reach Microsoft: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode == http.StatusOK {
		return Result{Name: name, Status: OK, Detail: tenant + " resolves"}
	}
	if code := aadCode(body); code == "AADSTS90002" {
		return Result{Name: name, Status: Problem, Detail: tenant + " does not exist",
			Fix: "Set MAILROOM_MICROSOFT_TENANT to a tenant id or an *.onmicrosoft.com domain, or " +
				"leave it unset for `common`, which accepts both personal and work accounts."}
	}
	return Result{Name: name, Status: Unknown,
		Detail: fmt.Sprintf("Microsoft answered %d for %s", resp.StatusCode, tenant)}
}

// microsoftSecret proves the client id and secret are a matching pair, by asking for a token
// the application is entitled to whether or not it holds any application permissions.
//
// It grants nothing usable: the token is for the application itself, and the registration
// mailroom creates has no application permissions at all. What matters is which of the two
// answers comes back.
func microsoftSecret(ctx context.Context, client *http.Client, tenant, clientID, secret string) Result {
	const name = "Microsoft client secret"

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{Name: name, Status: Unknown, Detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Name: name, Status: Unknown, Detail: "could not reach Microsoft: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var payload struct {
		AccessToken      string `json:"access_token"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)

	switch {
	case payload.AccessToken != "":
		return Result{Name: name, Status: OK, Detail: "the client id and secret are a matching pair"}
	case aadCode(body) == "AADSTS7000215":
		return Result{Name: name, Status: Problem, Detail: "Microsoft rejects this secret",
			Fix: "Generate a new one under Certificates & secrets and copy the Value column, not " +
				"the Secret ID beside it. A secret is shown once and cannot be read back later. " +
				"Check the expiry too: an expired secret is refused the same way."}
	case aadCode(body) == "AADSTS700016":
		return Result{Name: name, Status: Problem,
			Detail: "the directory does not have an application with this client id",
			Fix:    "The client id and the tenant disagree. Check both against the registration's overview."}
	default:
		// Conditional Access, tenant policy and a handful of other refusals all land here and
		// say nothing about whether the secret is right.
		return Result{Name: name, Status: Unknown,
			Detail: "Microsoft refused for a reason that does not answer the question: " + firstLine(payload.ErrorDescription)}
	}
}

// zohoRegions are the data centres Zoho serves accounts from. A client issued by one is not
// valid against another, and the refusal names neither the region nor the client.
var zohoRegions = map[string]bool{
	"com": true, "eu": true, "in": true, "com.au": true, "jp": true, "ca": true, "sa": true, "com.cn": true,
}

func zoho(ctx context.Context, client *http.Client, d Deployment) []Result {
	if d.ZohoClientID == "" {
		return []Result{{Name: "Zoho", Status: Skipped,
			Detail: "no client configured — linking a Zoho mailbox is switched off"}}
	}

	region := d.ZohoRegion
	if region == "" {
		region = "com"
	}

	out := []Result{}
	if zohoRegions[region] {
		out = append(out, Result{Name: "Zoho region", Status: OK, Detail: "accounts.zoho." + region})
	} else {
		out = append(out, Result{Name: "Zoho region", Status: Problem,
			Detail: region + " is not a Zoho data centre",
			Fix: "MAILROOM_ZOHO_REGION is one of com, eu, in, com.au, jp, ca, sa, com.cn — the " +
				"suffix of the Zoho domain you signed up on."})
	}

	// Same finding as Microsoft, verified against Zoho on 22 August 2026: the authorization
	// endpoint redirects to the sign-in page whatever the redirect URI is, and does the same
	// for a client id of `garbage`. Nothing about the client can be established before
	// somebody signs in.
	out = append(out, Result{Name: "Zoho redirect URI", Status: Unknown,
		Detail: strings.TrimSuffix(d.PublicURL, "/") + "/accounts/link/zoho/callback" +
			" — Zoho's authorization endpoint validates neither the redirect URI nor the client id before sign-in, so this cannot be verified from here",
		Fix: "Compare it by eye against the Redirect URIs on the client in the Zoho API console, " +
			"and check the client was created in the " + region + " data centre: a client from " +
			"another one is refused with an error naming neither."})

	return out
}

// imap has nothing to reach out to. It is reported anyway, because an operator reading a
// report that lists three providers should not be left wondering whether the fourth was
// checked and passed or simply forgotten.
func imap() Result {
	return Result{Name: "IMAP/SMTP", Status: Skipped,
		Detail: "nothing to check — an IMAP mailbox carries its own host and credentials and uses no OAuth client"}
}

// aadCode pulls the leading AADSTS code out of an error body. Microsoft puts it at the front
// of error_description, in prose, which is the only place it appears.
var aadPattern = regexp.MustCompile(`AADSTS\d+`)

func aadCode(body []byte) string {
	return aadPattern.FindString(string(body))
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	if s == "" {
		return "no explanation given"
	}
	return s
}
