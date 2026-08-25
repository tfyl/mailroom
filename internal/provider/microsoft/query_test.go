package microsoft

import (
	"context"
	"net/http"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// What Graph is asserted to receive for each canonical search.
//
// The split that shapes every cell below is Graph's two query mechanisms. $filter evaluates
// OData over indexed properties; $search runs the mailbox's full-text index over KQL. A term
// with free text in it goes to $search, everything else to $filter, and the two are never
// combined — not because Microsoft forbids it, but because Microsoft says nothing either way
// and separately warns that "Query parameters specified in a request might fail silently.
// This can be true for unsupported query parameters and for unsupported combinations of
// query parameters". A filter Graph drops in silence returns the whole mailbox and reads as
// an answer.
//
// The attachment term is the exception, and it is why that combination is a wire assertion
// rather than a refusal: hasAttachments has a KQL form and can ride inside the $search
// string itself, so nothing has to be dropped and nothing has to be refused.
func TestMicrosoftQueryTranslation(t *testing.T) {
	conformance.QueryTranslation(t, emitGraphSearch, map[string]conformance.Expectation{
		"free text": {
			Wire: `%24search=%22sasquatch%22`,
			Why: `Graph's worked example is GET /me/messages?$search="pizza", and without a ` +
				`property the search covers from, subject and body. ` +
				"https://learn.microsoft.com/en-us/graph/search-query-parameter",
		},
		"sender": {
			Wire: "from%3Ahedgehog%40example.com",
			Why: `from: is in Graph's table of KQL properties for messages, with the example ` +
				`$search="from:randiw". https://learn.microsoft.com/en-us/graph/search-query-parameter`,
		},
		"recipient": {
			Wire: "to%3Abadger%40example.com",
			Why: `to: is in the same table, with the example $search="to:randiw". ` +
				"https://learn.microsoft.com/en-us/graph/search-query-parameter",
		},
		"subject": {
			Wire: "subject%3Aaardvark",
			Why: `subject: is in the same table, with the example $search="subject:has". ` +
				"https://learn.microsoft.com/en-us/graph/search-query-parameter",
		},
		"unread": {
			Wire: "isRead+eq+false",
			Why: "one of the three documented $filter examples for messages is " +
				"~/me/mailFolders/inbox/messages?$filter=isRead eq false. " +
				"https://learn.microsoft.com/en-us/graph/filter-query-parameter",
		},
		"starred": {
			Wire: "flag%2FflagStatus+eq+%27flagged%27",
			Why: "flagStatus is a documented property of the followupFlag resource, but " +
				"filtering on it is documented nowhere — there is no filterable-property list " +
				"for message at all. Search therefore checks the results as well as asking, " +
				"the same way GetThread does with conversationId. " +
				"https://learn.microsoft.com/en-us/graph/api/resources/followupflag",
		},
		"has attachment": {
			Wire: "hasAttachments+eq+true",
			Why: "hasAttachments is a documented property of the message resource; filtering " +
				"on it is not among the published $filter examples, so the results are " +
				"checked as well. https://learn.microsoft.com/en-us/graph/api/resources/message",
		},
		"unread alongside free text": {
			Refused: true,
			Why: "whether $search and $filter compose on messages is documented nowhere, and " +
				"an unsupported combination of query parameters is documented as something " +
				"that might fail silently. " +
				"https://learn.microsoft.com/en-us/graph/known-issues",
		},
		"starred alongside free text": {
			Refused: true,
			Why: "the same silent-failure risk, over a filter that is already undocumented on " +
				"its own. https://learn.microsoft.com/en-us/graph/known-issues",
		},
		"attachment alongside free text": {
			Wire: "hasAttachments%3Atrue",
			Why: `hasAttachments has a KQL form, so it rides inside $search rather than ` +
				`needing $filter beside it. Microsoft's property table spells it hasAttachment ` +
				`and the example in the same row says $search="hasAttachments:true"; the ` +
				`example is the half that has plainly been run. ` +
				"https://learn.microsoft.com/en-us/graph/search-query-parameter",
		},
	})
}

func emitGraphSearch(t *testing.T, q mmail.Query) (string, error) {
	t.Helper()

	var request string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		request = r.URL.RequestURI()
		writeJSON(t, w, messagePage{})
	})

	_, err := p.Search(context.Background(), q, "")
	return request, err
}
