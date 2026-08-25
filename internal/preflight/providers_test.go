package preflight

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// router answers by URL substring, because the Microsoft checks talk to two endpoints and
// they have to be told apart. Bodies below are the shapes Microsoft returned on
// 22 August 2026, with the identifiers replaced.
type router struct {
	routes map[string]stubResponse
}

type stubResponse struct {
	status int
	body   string
}

func (r router) RoundTrip(req *http.Request) (*http.Response, error) {
	for fragment, resp := range r.routes {
		if strings.Contains(req.URL.String(), fragment) {
			return &http.Response{
				StatusCode: resp.status,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(resp.body)),
			}, nil
		}
	}
	return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
}

const (
	// Trimmed of the trace and correlation ids, which vary per request.
	tenantNotFound = `{"error":"invalid_request","error_description":"AADSTS90002: Tenant 'no-such-tenant' not found. Check to make sure you have the correct tenant ID and are signing into the correct cloud."}`
	badSecret      = `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, not the client secret ID."}`
	appNotInTenant = `{"error":"unauthorized_client","error_description":"AADSTS700016: Application with identifier '00000000-0000-0000-0000-000000000000' was not found in the directory."}`
	// What /common answers a client-credentials request with — the reason the secret check is
	// skipped on a multi-tenant segment rather than run and misread.
	conditionalAccess = `{"error":"invalid_grant","error_description":"AADSTS53003: Access has been blocked by Conditional Access policies."}`
	tokenIssued       = `{"token_type":"Bearer","expires_in":3599,"access_token":"redacted-not-a-real-token"}`
	discoveryOK       = `{"issuer":"https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0"}`
)

const testGUID = "11111111-2222-3333-4444-555555555555"

func find(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %d results", name, len(results))
	return Result{}
}

func TestPublicURLRejectsWhatProvidersWillNotMatch(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		want      Status
	}{
		{"ordinary https", "https://mail.example.com", OK},
		{"localhost may be http", "http://localhost:8080", OK},
		// Not a problem: config.Load trims it before this ever sees it.
		{"trailing slash is harmless", "https://mail.example.com/", OK},
		{"a path", "https://mail.example.com/mailroom", Problem},
		{"http on a real host", "http://mail.example.com", Problem},
		{"unset", "", Problem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicURL(tc.url); got.Status != tc.want {
				t.Fatalf("publicURL(%q) = %s (%s), want %s", tc.url, got.Status, got.Detail, tc.want)
			}
		})
	}
}

func TestMicrosoftUnconfiguredIsSkippedNotFailed(t *testing.T) {
	got := microsoft(context.Background(), &http.Client{}, Deployment{PublicURL: "https://mail.example.com"})
	if len(got) != 1 || got[0].Status != Skipped {
		t.Fatalf("an instance not using Microsoft should report one skipped check, got %+v", got)
	}
}

func TestMicrosoftClientIDMustBeAGUID(t *testing.T) {
	client := &http.Client{Transport: router{routes: map[string]stubResponse{
		"openid-configuration": {http.StatusOK, discoveryOK},
	}}}
	d := Deployment{PublicURL: "https://mail.example.com", MicrosoftClientID: "not-a-guid", MicrosoftTenant: "common"}

	got := find(t, microsoft(context.Background(), client, d), "Microsoft client id")
	if got.Status != Problem {
		t.Fatalf("a client id that is not a GUID should fail, got %s (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Fix, "Secret ID") {
		t.Error("the fix should warn about the neighbouring ids that are also GUIDs")
	}
}

func TestMicrosoftTenantThatDoesNotExist(t *testing.T) {
	client := &http.Client{Transport: router{routes: map[string]stubResponse{
		"openid-configuration": {http.StatusBadRequest, tenantNotFound},
	}}}
	d := Deployment{PublicURL: "https://mail.example.com", MicrosoftClientID: testGUID, MicrosoftTenant: "no-such-tenant"}

	got := find(t, microsoft(context.Background(), client, d), "Microsoft tenant")
	if got.Status != Problem {
		t.Fatalf("AADSTS90002 should be a problem, got %s (%s)", got.Status, got.Detail)
	}
}

func TestMicrosoftSecretOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want Status
	}{
		{"a matching pair", tokenIssued, http.StatusOK, OK},
		{"wrong secret", badSecret, http.StatusUnauthorized, Problem},
		{"client id not in this tenant", appNotInTenant, http.StatusBadRequest, Problem},
		{"refused for an unrelated reason", conditionalAccess, http.StatusBadRequest, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: router{routes: map[string]stubResponse{
				"openid-configuration": {http.StatusOK, discoveryOK},
				"oauth2/v2.0/token":    {tc.code, tc.body},
			}}}
			d := Deployment{
				PublicURL:             "https://mail.example.com",
				MicrosoftClientID:     testGUID,
				MicrosoftClientSecret: "whatever",
				MicrosoftTenant:       testGUID,
			}
			got := find(t, microsoft(context.Background(), client, d), "Microsoft client secret")
			if got.Status != tc.want {
				t.Fatalf("got %s (%s), want %s", got.Status, got.Detail, tc.want)
			}
		})
	}
}

// The check cannot run against a tenant segment naming a class of directory, and the point of
// this test is that it is not attempted and misread: /common answers with AADSTS53003, which
// says nothing about the secret.
func TestMicrosoftSecretIsSkippedOnAMultiTenantSegment(t *testing.T) {
	for _, tenant := range []string{"common", "organizations", "consumers", "COMMON"} {
		t.Run(tenant, func(t *testing.T) {
			client := &http.Client{Transport: router{routes: map[string]stubResponse{
				"openid-configuration": {http.StatusOK, discoveryOK},
				"oauth2/v2.0/token":    {http.StatusBadRequest, conditionalAccess},
			}}}
			d := Deployment{
				PublicURL:             "https://mail.example.com",
				MicrosoftClientID:     testGUID,
				MicrosoftClientSecret: "whatever",
				MicrosoftTenant:       tenant,
			}
			got := find(t, microsoft(context.Background(), client, d), "Microsoft client secret")
			if got.Status != Skipped {
				t.Fatalf("got %s (%s), want skipped", got.Status, got.Detail)
			}
		})
	}
}

// Both of these are Unknown on purpose. Entra and Zoho answer a registered and an
// unregistered redirect URI identically before sign-in, so a check claiming to have verified
// one would be claiming something it cannot know — which is the failure this whole package
// exists to avoid.
func TestRedirectURIsThatCannotBeVerifiedSaySo(t *testing.T) {
	client := &http.Client{Transport: router{routes: map[string]stubResponse{
		"openid-configuration": {http.StatusOK, discoveryOK},
		"oauth2/v2.0/token":    {http.StatusOK, tokenIssued},
	}}}
	d := Deployment{
		PublicURL:         "https://mail.example.com",
		MicrosoftClientID: testGUID, MicrosoftClientSecret: "whatever", MicrosoftTenant: testGUID,
		ZohoClientID: "1000.EXAMPLE", ZohoRegion: "eu",
	}

	ms := find(t, microsoft(context.Background(), client, d), "Microsoft redirect URI")
	if ms.Status != Unknown {
		t.Fatalf("Microsoft redirect URI = %s, want unknown", ms.Status)
	}
	if !strings.Contains(ms.Detail, "https://mail.example.com/accounts/link/microsoft/callback") {
		t.Errorf("the detail should carry the exact URI to compare, got %q", ms.Detail)
	}

	z := find(t, zoho(context.Background(), client, d), "Zoho redirect URI")
	if z.Status != Unknown {
		t.Fatalf("Zoho redirect URI = %s, want unknown", z.Status)
	}
	if !strings.Contains(z.Detail, "https://mail.example.com/accounts/link/zoho/callback") {
		t.Errorf("the detail should carry the exact URI to compare, got %q", z.Detail)
	}
}

func TestZohoRegion(t *testing.T) {
	client := &http.Client{}
	for _, tc := range []struct {
		region string
		want   Status
	}{
		{"eu", OK}, {"com", OK}, {"com.au", OK}, {"co.uk", Problem}, {"", OK},
	} {
		t.Run("region "+tc.region, func(t *testing.T) {
			d := Deployment{PublicURL: "https://mail.example.com", ZohoClientID: "1000.EXAMPLE", ZohoRegion: tc.region}
			got := find(t, zoho(context.Background(), client, d), "Zoho region")
			if got.Status != tc.want {
				t.Fatalf("region %q = %s (%s), want %s", tc.region, got.Status, got.Detail, tc.want)
			}
		})
	}
}

// A provider nobody configured is not a failure, and an operator has to be able to tell that
// apart from one that failed.
func TestUnconfiguredProvidersDoNotFailTheReport(t *testing.T) {
	results := []Result{
		{Name: "Google", Status: Skipped, Detail: "no client configured"},
		{Name: "IMAP/SMTP", Status: Skipped, Detail: "nothing to check"},
		{Name: "Microsoft redirect URI", Status: Unknown, Detail: "cannot be verified"},
	}
	report, problems := Report(results)
	if problems {
		t.Error("skipped and unknown checks must not report the deployment as broken")
	}
	if !strings.Contains(report, "--    Google") {
		t.Errorf("skipped checks need their own marker, got:\n%s", report)
	}
}

func TestIMAPIsReportedRatherThanOmitted(t *testing.T) {
	if got := imap(); got.Status != Skipped || !strings.Contains(got.Detail, "no OAuth client") {
		t.Fatalf("IMAP should appear in the report explaining why there is nothing to check, got %+v", got)
	}
}
