package zoho

import (
	"errors"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// A refusal from the token endpoint carries no HTTP status of its own, so before this it fell
// through to a generic failure in both directions: a dead refresh token was never reported as
// needing a re-link, leaving the mailbox marked healthy while every call failed obscurely, and
// a throttle was reported as permanent, so a client had no reason to wait.
func TestZohoTokenFailuresAreClassified(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"}}

	for _, tc := range []struct {
		name       string
		err        error
		wantReauth bool
		wantRetry  bool
	}{
		{
			// The wording Zoho actually returned, under an errorCode of Access Denied —
			// which is why the description is read and not just the code.
			name: "throttled at the token endpoint",
			err: &oauth2.RetrieveError{
				ErrorCode:        "Access Denied",
				ErrorDescription: "You have made too many requests continuously. Please try again after some time.",
			},
			wantRetry: true,
		},
		{
			name:      "token endpoint unwell",
			err:       &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadGateway}},
			wantRetry: true,
		},
		{
			name:       "refresh token finished",
			err:        &oauth2.RetrieveError{ErrorCode: "invalid_grant", Body: []byte(`{"error":"invalid_grant"}`)},
			wantReauth: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.wrap("GET /messages", 0, tc.err)

			if isReauth := errors.Is(got, mmail.ErrNeedsReauth); isReauth != tc.wantReauth {
				t.Fatalf("needs_reauth=%v, want %v (got %v)", isReauth, tc.wantReauth, got)
			}
			if !tc.wantRetry {
				return
			}
			var provErr *mmail.ProviderError
			if !errors.As(got, &provErr) || !provErr.Retryable {
				t.Fatalf("a transient token failure must be retryable, got %v", got)
			}
		})
	}
}
