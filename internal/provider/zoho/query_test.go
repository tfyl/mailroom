package zoho

import (
	"context"
	"net/http"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// What Zoho is asserted to put on the wire for each canonical search.
//
// The two endpoints are the whole story here. Zoho documents the listing endpoint,
// /messages/view, as taking status, flagid, labelid and attachedMails among sixteen
// parameters; it documents the search endpoint, /messages/search, as taking searchKey,
// receivedTime, start, limit and includeto, and nothing else. A filter parameter sent to the
// search endpoint is therefore not an error — it is a parameter that does nothing, and the
// answer is the whole mailbox with the search terms applied and the filter silently dropped.
//
// So every combination of a filter with search terms is a refusal, and every filter on its
// own goes to the listing endpoint where Zoho reads it.
//
// The free-text and field terms assert the value rather than the syntax around it. The
// syntax itself — `entire:` for free text, `::` between conditions — is the subject of its
// own change, and pinning a form here that is known to be wrong would be worse than pinning
// the one thing that is true either way: the value the caller asked for has to reach Zoho.
func TestZohoQueryTranslation(t *testing.T) {
	conformance.QueryTranslation(t, emitZohoSearch, map[string]conformance.Expectation{
		"free text": {
			Wire: "sasquatch",
			Why: "Zoho's search syntax reference gives entire: for a whole-message search; " +
				"whatever the field, the term itself has to travel in searchKey. " +
				"https://www.zoho.com/mail/help/search-syntax.html",
		},
		"sender": {
			Wire: "hedgehog%40example.com",
			Why: "sender: is one of the documented searchKey conditions. " +
				"https://www.zoho.com/mail/help/search-syntax.html",
		},
		"recipient": {
			Wire: "badger%40example.com",
			Why: "to: is one of the documented searchKey conditions. " +
				"https://www.zoho.com/mail/help/search-syntax.html",
		},
		"subject": {
			Wire: "aardvark",
			Why: "subject: is one of the documented searchKey conditions. " +
				"https://www.zoho.com/mail/help/search-syntax.html",
		},
		"unread": {
			Wire: "status=unread",
			Why: "the listing endpoint documents status with the values read, unread and all. " +
				"https://www.zoho.com/mail/help/api/get-emails-list.html",
		},
		"starred": {
			Wire: "flagid=3",
			Why: "the listing endpoint documents flagid as 0 flag_not_set, 1 info, " +
				"2 important, 3 followup. https://www.zoho.com/mail/help/api/get-emails-list.html",
		},
		"has attachment": {
			Wire: "attachedMails=true",
			Why: "the listing endpoint documents attachedMails as retrieving only mail with " +
				"attachments. https://www.zoho.com/mail/help/api/get-emails-list.html",
		},
		"unread alongside free text": {
			Refused: true,
			Why: "the search endpoint's documented parameters are searchKey, receivedTime, " +
				"start, limit and includeto; status is not among them, and Zoho's search " +
				"syntax has no condition for read state either. " +
				"https://www.zoho.com/mail/help/api/get-search-emails.html",
		},
		"starred alongside free text": {
			Refused: true,
			Why: "flagid is a listing-endpoint parameter, and the nearest search condition, " +
				"has:flags, matches info and important as well as followup. " +
				"https://www.zoho.com/mail/help/api/get-search-emails.html",
		},
		"attachment alongside free text": {
			Refused: true,
			Why: "attachedMails is a listing-endpoint parameter. has:attachment is the " +
				"documented search condition for this and is not wired up yet, so the honest " +
				"answer is a refusal rather than a filter Zoho will not read. " +
				"https://www.zoho.com/mail/help/api/get-search-emails.html",
		},
	})
}

// emitZohoSearch runs one search against a stub and reports the request line it produced.
func emitZohoSearch(t *testing.T, q mmail.Query) (string, error) {
	t.Helper()

	var request string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		request = r.URL.RequestURI()
		writeEnvelope(t, w, []message{})
	})

	_, err := p.Search(context.Background(), q, "")
	return request, err
}
