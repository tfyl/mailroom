package gmail

import (
	"errors"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// A missing message and a transport failure have to be distinguishable, and Gmail says
// missing in two ways: 404 for a message that is not there, and 400 invalidArgument for an id
// it will not parse. The second is the one a caller actually hits — a draft id used as a
// message id, an id from another mailbox, an id a model invented — and it was arriving as a
// provider error, which reads as "the mail service is unwell" rather than "no such message".
//
// The wordings are what a live mailbox returned.
func TestGmailReportsAnUnreadableIDAsNotFound(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"}}

	for _, tc := range []struct {
		name    string
		err     error
		wantNF  bool
		wantMsg string
	}{
		{
			name:   "message that is not there",
			err:    &googleapi.Error{Code: http.StatusNotFound, Message: "Not Found"},
			wantNF: true,
		},
		{
			name:   "id Gmail will not parse",
			err:    &googleapi.Error{Code: http.StatusBadRequest, Message: "Invalid id value"},
			wantNF: true,
		},
		{
			name:   "batch route, plural wording",
			err:    &googleapi.Error{Code: http.StatusBadRequest, Message: "Invalid ids value"},
			wantNF: true,
		},
		{
			// The narrowness matters: a 400 is also how Gmail reports a request mailroom
			// built wrong. Reading that as not-found would turn a bug here into an empty
			// result that looks like an answer.
			name:   "a request mailroom got wrong",
			err:    &googleapi.Error{Code: http.StatusBadRequest, Message: "Invalid label: NOT_A_LABEL"},
			wantNF: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.wrap("get_message", tc.err)
			isNotFound := errors.Is(got, mmail.ErrNotFound)
			if isNotFound != tc.wantNF {
				t.Fatalf("not_found=%v, want %v (got %v)", isNotFound, tc.wantNF, got)
			}
			if !tc.wantNF && mmail.Code(got) == "not_found" {
				t.Errorf("a malformed request must not be reported as missing mail: %v", got)
			}
		})
	}
}
