package gmail

import (
	"errors"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// ErrNeedsReauth is not a report, it is a decision that sticks: Gate.Observe writes
// needs_reauth on the account and every later call is refused before it reaches Google. So a
// transient refusal from the token endpoint must not produce it — a rate limit would
// otherwise lock a healthy mailbox until a human re-links it.
//
// The other direction matters just as much. A mailbox whose refresh token really is finished
// has to say so, or a client retries forever and nobody is told.
func TestTokenFailuresAreClassified(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"}}

	for _, tc := range []struct {
		name        string
		err         *oauth2.RetrieveError
		wantReauth  bool
		wantRetry   bool
		wantRetryIn int
	}{
		{
			name:       "a revoked refresh token",
			err:        &oauth2.RetrieveError{ErrorCode: "invalid_grant", Response: &http.Response{StatusCode: 400}},
			wantReauth: true,
		},
		{
			name:       "a client that no longer exists",
			err:        &oauth2.RetrieveError{ErrorCode: "invalid_client", Response: &http.Response{StatusCode: 401}},
			wantReauth: true,
		},
		{
			name:        "throttled",
			err:         &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
			wantRetry:   true,
			wantRetryIn: 60,
		},
		{
			name:        "the token endpoint is unwell",
			err:         &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusInternalServerError}},
			wantRetry:   true,
			wantRetryIn: 30,
		},
		{
			name:        "throttled, reported only in the code",
			err:         &oauth2.RetrieveError{ErrorCode: "rate_limit_exceeded"},
			wantRetry:   true,
			wantRetryIn: 60,
		},
		{
			// The conservative default. Something unrecognised is treated as a dead
			// credential, because a mailbox needing a human is worse left looking healthy.
			name:       "an unrecognised refusal",
			err:        &oauth2.RetrieveError{ErrorCode: "something_new"},
			wantReauth: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.wrap("get_message", tc.err)

			if isReauth := errors.Is(got, mmail.ErrNeedsReauth); isReauth != tc.wantReauth {
				t.Fatalf("needs_reauth=%v, want %v (got %v)", isReauth, tc.wantReauth, got)
			}
			if !tc.wantRetry {
				return
			}
			var provErr *mmail.ProviderError
			if !errors.As(got, &provErr) {
				t.Fatalf("want a provider error carrying a retry, got %v", got)
			}
			if !provErr.Retryable {
				t.Errorf("a transient token failure must be retryable: %v", got)
			}
			if provErr.RetryIn != tc.wantRetryIn {
				t.Errorf("RetryIn = %d, want %d", provErr.RetryIn, tc.wantRetryIn)
			}
		})
	}
}
