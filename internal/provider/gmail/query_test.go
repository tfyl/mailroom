package gmail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// What Gmail is asserted to receive for each canonical search.
//
// Gmail is the provider the canonical Query was shaped around, so every term here is served
// and none is refused. That is worth pinning precisely because it is the easy case: the
// expectations elsewhere in this suite are the interesting ones, and they only mean something
// if the provider they were written against is held to its own.
//
// The one term that does not travel in the query string is the label, and it is not in the
// canonical list because mail_search cannot yet ask for one. It has its own test below.
func TestGmailQueryTranslation(t *testing.T) {
	conformance.QueryTranslation(t, emitGmailSearch, map[string]conformance.Expectation{
		"free text": {
			Wire: "q=sasquatch",
			Why: "users.messages.list documents q as supporting the same query format as the " +
				"Gmail search box. " +
				"https://developers.google.com/gmail/api/reference/rest/v1/users.messages/list",
		},
		"sender": {
			Wire: "q=from%3Ahedgehog%40example.com",
			Why: "from: is a documented Gmail search operator. " +
				"https://support.google.com/mail/answer/7190",
		},
		"recipient": {
			Wire: "q=to%3Abadger%40example.com",
			Why: "to: is a documented Gmail search operator. " +
				"https://support.google.com/mail/answer/7190",
		},
		"subject": {
			Wire: "q=subject%3Aaardvark",
			Why: "subject: is a documented Gmail search operator. " +
				"https://support.google.com/mail/answer/7190",
		},
		"unread": {
			Wire: "q=is%3Aunread",
			Why: "is:unread is documented under searching by status. " +
				"https://support.google.com/mail/answer/7190",
		},
		"starred": {
			Wire: "q=is%3Astarred",
			Why: "is:starred is documented under searching by status. " +
				"https://support.google.com/mail/answer/7190",
		},
		"has attachment": {
			Wire: "q=has%3Aattachment",
			Why: "has:attachment is documented under finding mail that includes attachments. " +
				"https://support.google.com/mail/answer/7190",
		},
		"unread alongside free text": {
			Wire: "q=sasquatch+is%3Aunread",
			Why: "operators are combined by juxtaposition in the Gmail search box, and q takes " +
				"the same syntax. https://support.google.com/mail/answer/7190",
		},
		"starred alongside free text": {
			Wire: "q=sasquatch+is%3Astarred",
			Why: "operators combine by juxtaposition, as above. " +
				"https://support.google.com/mail/answer/7190",
		},
		"attachment alongside free text": {
			Wire: "q=sasquatch+has%3Aattachment",
			Why: "operators combine by juxtaposition, as above. " +
				"https://support.google.com/mail/answer/7190",
		},
	})
}

// A label is scoped with labelIds, not with the label: operator, and the difference is the
// difference between an answer and an empty one.
//
// The two parameters take different things. labelIds takes the ids users.labels.list returns
// — "Only return messages with labels that match all of the specified label IDs" — while
// label: is the search-box operator, whose documented examples are display names
// (label:friends, label:important). A user label's id is not its name, and Gmail does not
// refuse a label token it cannot resolve: it matches nothing. So a label-scoped search used
// to come back empty and successful, which is the one answer a caller cannot tell apart from
// an empty mailbox.
func TestGmailScopesALabelByIDRatherThanByName(t *testing.T) {
	request, err := emitGmailSearch(t, mmail.Query{Label: "Label_1234567890123456789"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if !strings.Contains(request, "labelIds=Label_1234567890123456789") {
		t.Errorf("the label must be scoped with labelIds, which takes the id: %s", request)
	}
	if strings.Contains(request, "label%3A") {
		t.Errorf("the label: operator takes a display name, so an id sent to it matches "+
			"nothing and the search answers empty: %s", request)
	}
}

// emitGmailSearch runs one search against a stub Gmail and reports the request line it
// produced. The stub answers with no messages, so nothing follows the listing call.
func emitGmailSearch(t *testing.T, q mmail.Query) (string, error) {
	t.Helper()

	var request string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"resultSizeEstimate":0}`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	svc, err := gmail.NewService(ctx,
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("building a stub gmail service: %v", err)
	}

	p := &Provider{svc: svc, account: mmail.Account{ID: "acct_1", Alias: "work"}}
	_, searchErr := p.Search(ctx, q, "")
	return request, searchErr
}
