package zoho

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The stubs in this file are the bodies a live Zoho mailbox actually returned, trimmed to the
// fields mailroom reads. They exist because the three bugs they cover were all invisible to a
// stub that answered whatever it was told to: each one is a place where mailroom's idea of
// the wire format and Zoho's disagreed, and only a real account could say so. Running the
// live suite is how they were found; these are how they stay found.

// liveMessageID is the id of a real message, kept at its true width. Nineteen digits is more
// than a float64 carries, which is load-bearing for the first test below.
const liveMessageID = "1234567890123456789"

const liveFolderID = "860000000000000001"

// listingJSON is one element of the /messages/view and /messages/search arrays. Every scalar
// is quoted, including the two identifiers.
const listingJSON = `{
	"messageId": "1234567890123456789",
	"folderId": "860000000000000001",
	"subject": "Quarterly invoice",
	"sender": "Vendor",
	"fromAddress": "notifications@vendor.example",
	"toAddress": "&lt;you@example.com&gt;",
	"ccAddress": "Not Provided",
	"summary": "Quarterly invoice",
	"receivedTime": "1700000000000",
	"sentDateInGMT": "1699999000000",
	"status": "0",
	"status2": "0",
	"flagid": "flag_not_set",
	"hasAttachment": "0",
	"priority": "3",
	"size": "19803",
	"calendarType": 0
}`

// contentJSON is what /folders/{f}/messages/{m}/content answers with: two fields, and the id
// as a bare number rather than the quoted form every other endpoint uses.
const contentJSON = `{"messageId": 1234567890123456789, "content": "<html><body>hello</body></html>"}`

func envelopeOf(data string) string {
	return `{"status":{"code":200,"description":"success"},"data":` + data + `}`
}

// zohoStub answers the two endpoints Get reads, and records what it was asked for.
func zohoStub(t *testing.T, seen *[]string) *Provider {
	t.Helper()
	return testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.URL.Path)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/details"):
			_, _ = w.Write([]byte(envelopeOf(listingJSON)))
		case strings.HasSuffix(r.URL.Path, "/content"):
			_, _ = w.Write([]byte(envelopeOf(contentJSON)))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// The fatal one. Search hands back an id, the caller fetches it, and the fetch has to work.
//
// It did not: the content endpoint spells messageId as a JSON number and the struct decoded
// it as a string, so every id search returned failed with "cannot unmarshal number into Go
// struct field" the moment anything tried to open it.
func TestZohoResolvesAnIDWhoseMessageIDCameBackAsANumber(t *testing.T) {
	p := zohoStub(t, nil)

	got, err := p.Get(context.Background(), p.scoped(liveFolderID, liveMessageID))
	if err != nil {
		t.Fatalf("an id search returns must be resolvable by Get: %v", err)
	}
	if got.ID.Native != liveFolderID+"/"+liveMessageID {
		t.Errorf("Get returned id %q, want the one it was asked for", got.ID.Native)
	}
}

// The id has to survive being a number, not merely be accepted as one.
//
// Zoho's ids are nineteen digits and a float64 carries fifteen or sixteen, so decoding one as
// a number and printing it back yields a different id — 1234567890123456768 for the message
// above. That is worse than the decode error it replaces, because the request that follows
// succeeds in form and names a message that does not exist.
func TestZohoIdentifiersKeepEveryDigitTheyArrivedWith(t *testing.T) {
	for _, form := range []string{`"` + liveMessageID + `"`, liveMessageID} {
		var id flexString
		if err := id.UnmarshalJSON([]byte(form)); err != nil {
			t.Fatalf("decoding %s: %v", form, err)
		}
		if id.String() != liveMessageID {
			t.Errorf("decoding %s gave %q, want %q", form, id, liveMessageID)
		}
	}

	// A shape that is neither spelling is a change in the response rather than a third way of
	// writing the same value, and putting whatever it is into an id would be a guess.
	var id flexString
	if err := id.UnmarshalJSON([]byte(`{"id":1}`)); err == nil {
		t.Error("an object is not an identifier and must be refused rather than coerced")
	}
	if err := id.UnmarshalJSON([]byte(`null`)); err != nil {
		t.Errorf("an absent identifier is empty, not an error: %v", err)
	}
}

// Opening a message has to return the message. The content endpoint carries a body and an id
// and nothing else, so reading only it gave mail_get an empty envelope around some text and
// a date of year 1 — technically a successful fetch, and unusable.
func TestZohoGetReturnsTheMessageAndNotJustItsBody(t *testing.T) {
	var seen []string
	p := zohoStub(t, &seen)

	got, err := p.Get(context.Background(), p.scoped(liveFolderID, liveMessageID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Quarterly invoice" {
		t.Errorf("subject = %q, want the one the details endpoint reports", got.Subject)
	}
	if got.From.Email != "notifications@vendor.example" {
		t.Errorf("from = %q, want the sender the details endpoint reports", got.From.Email)
	}
	if got.Date.IsZero() {
		t.Error("date is zero; the aggregator sorts on it and would order this message arbitrarily")
	}
	if !strings.Contains(got.Body.HTML, "hello") {
		t.Errorf("body = %q, want the content endpoint's html", got.Body.HTML)
	}
	if len(seen) != 2 {
		t.Errorf("expected one request for metadata and one for the body, got %v", seen)
	}
}

// Zoho answers 400 Invalid Input where the other three providers answer 404. Without the
// mapping a caller cannot tell a message that is not there from a mailbox it cannot reach,
// and an agent will report a transport failure as deleted mail or the reverse.
//
// Both wordings are here because Zoho uses both, on the same absent id, depending on which
// endpoint was asked. Get reads /details first, and /details is the one that never names the
// id back — so a mapping that recognised only the named form passed against a stub of the
// content endpoint and still failed the live suite.
func TestZohoReportsAMissingMessageAsNotFound(t *testing.T) {
	for name, moreInfo := range map[string]string{
		"the wording /content uses": "Message id 999999999999999999 is invalid",
		"the wording /details uses": "messageId is invalid",
	} {
		t.Run(name, func(t *testing.T) {
			p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":{"code":400,"description":"Invalid Input"},` +
					`"data":{"moreInfo":"` + moreInfo + `"}}`))
			})

			_, err := p.Get(context.Background(), p.scoped("0", "999999999999999999"))
			if !errors.Is(err, mmail.ErrNotFound) {
				t.Fatalf("want ErrNotFound so a caller can tell this from a transport failure, got %T: %v", err, err)
			}
		})
	}
}

// The mapping must not reach past a request that named one message.
//
// A search or a listing cannot be answering "that message is gone", whatever prose comes
// back, so a 400 from one stays a provider error. Without this the not-found mapping would be
// a way for a query mailroom got wrong to come back as an empty page — which is the failure
// the search-syntax bug already inflicted once, wearing a different disguise.
func TestZohoNeverReadsAListingFailureAsAMissingMessage(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":{"code":400,"description":"Invalid Input"},` +
			`"data":{"moreInfo":"messageId is invalid"}}`))
	})

	_, err := p.Search(context.Background(), mmail.Query{Limit: 10}, "")
	if err == nil {
		t.Fatal("a refused search must not be reported as success")
	}
	if errors.Is(err, mmail.ErrNotFound) {
		t.Error("a listing that zoho refused is a broken query, not a message that is absent; " +
			"reporting not-found here hides the bug that caused it")
	}
}

// The other half of the same decision, and the one that matters more.
//
// Zoho also answers 400 for a request it could not parse, which is a bug in mailroom rather
// than a missing message — this provider shipped two such bugs, and reading every 400 as
// not-found would have turned both into an empty mailbox that looked like an answer. Each
// body below was returned by the live account.
func TestZohoKeepsAMalformedRequestDistinctFromAMissingMessage(t *testing.T) {
	for name, body := range map[string]string{
		"a parameter the endpoint does not take": `{"data":{"errorCode":"EXTRA_PARAM_FOUND",` +
			`"moreInfo":"threadId Extra paramters given"},"status":{"code":400,"description":"Invalid Input"}}`,
		"a parameter of the wrong type": `{"data":{"errorCode":"DATATYPE_NOT_MATCHED",` +
			`"moreInfo":"includeto Input datatype does not match"},"status":{"code":400,"description":"Invalid Input"}}`,
		"a parameter sent twice": `{"data":{"errorCode":"MORE_THAN_MAX_OCCURANCE",` +
			`"moreInfo":"searchKey More than minimum occurence"},"status":{"code":400,"description":"Invalid Input"}}`,
		"a complaint about some other message": `{"status":{"code":400,"description":"Invalid Input"},` +
			`"data":{"moreInfo":"Message id 111111111111111111 is invalid"}}`,
		"prose about something else entirely": `{"status":{"code":400,"description":"Invalid Input"},` +
			`"data":{"moreInfo":"The input passed is invalid"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(body))
			})

			_, err := p.Get(context.Background(), p.scoped("0", "999999999999999999"))
			if err == nil {
				t.Fatal("a refusal must not be reported as success")
			}
			if errors.Is(err, mmail.ErrNotFound) {
				t.Errorf("this is a request zoho could not answer, not a message that is absent; "+
					"reporting it as not-found hides the bug that caused it: %s", body)
			}
		})
	}
}

// A thread must contain at least the message it was reached from.
//
// Zoho will not say which thread a message is in, so mailroom guesses that the message's own
// id is the thread id — right for the message that started a thread, wrong for every reply,
// and for a reply Zoho answers with an empty array. That was being returned as the thread,
// which reads as "there is no conversation here" rather than "this could not be looked up".
func TestZohoThreadContainsTheMessageItWasReachedFrom(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/details"):
			_, _ = w.Write([]byte(envelopeOf(listingJSON)))
		case strings.HasSuffix(r.URL.Path, "/content"):
			_, _ = w.Write([]byte(envelopeOf(contentJSON)))
		case strings.HasSuffix(r.URL.Path, "/messages/view"):
			if got := r.URL.Query().Get("threadId"); got != liveMessageID {
				t.Errorf("thread lookup asked for threadId=%q, want the message id", got)
			}
			_, _ = w.Write([]byte(envelopeOf(`[]`)))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	})

	thread, err := p.GetThread(context.Background(), p.scoped(liveFolderID, liveMessageID))
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if len(thread.Messages) == 0 {
		t.Fatal("an empty thread reads as a conversation with nothing in it; it must hold the anchor")
	}
	if thread.Messages[0].ID.Native != liveFolderID+"/"+liveMessageID {
		t.Errorf("thread anchored on %q, want the message it was reached from", thread.Messages[0].ID.Native)
	}
	if !thread.Derived {
		t.Error("the grouping was inferred from an id mailroom guessed at, and has to say so")
	}
	if thread.Subject == "" {
		t.Error("a thread with a message in it has that message's subject")
	}
}

// When the guess does land — the message did start the thread — Zoho answers with the whole
// conversation, including the anchor. It must appear once.
func TestZohoThreadMergesZohosMembersWithoutDuplicatingTheAnchor(t *testing.T) {
	const reply = `{
		"messageId": "1234567890123456790",
		"folderId": "860000000000000001",
		"threadId": "1234567890123456789",
		"threadCount": "0",
		"subject": "Re: Quarterly invoice",
		"fromAddress": "someone@example.com",
		"receivedTime": "1700000060000"
	}`

	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/details"):
			_, _ = w.Write([]byte(envelopeOf(listingJSON)))
		case strings.HasSuffix(r.URL.Path, "/content"):
			_, _ = w.Write([]byte(envelopeOf(contentJSON)))
		case strings.HasSuffix(r.URL.Path, "/messages/view"):
			_, _ = w.Write([]byte(envelopeOf(`[` + listingJSON + `,` + reply + `]`)))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	})

	thread, err := p.GetThread(context.Background(), p.scoped(liveFolderID, liveMessageID))
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("want the anchor and the reply, got %d messages", len(thread.Messages))
	}
	seen := map[string]int{}
	for _, m := range thread.Messages {
		seen[m.ID.String()]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("message %s appears %d times; the anchor is fetched separately and must not be repeated", id, n)
		}
	}
	if !thread.Messages[0].Date.Before(thread.Messages[1].Date) &&
		!thread.Messages[0].Date.Equal(thread.Messages[1].Date) {
		t.Error("a conversation reads oldest first")
	}
	// The anchor carries the body, which only the per-message fetch supplies. Merging must
	// not overwrite it with the listing entry for the same message.
	if !strings.Contains(thread.Messages[0].Body.HTML, "hello") {
		t.Error("the anchor lost its body when Zoho's own listing was merged in")
	}
}

// A listing entry that carries a real threadId must be reported under it rather than under
// the message's own id, or a caller reaching the thread from a reply asks for the wrong one.
func TestZohoPrefersARealThreadIDWhenZohoSuppliesOne(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work"}}

	withThread := p.convert(message{MessageID: "2", FolderID: "10", ThreadID: "1"})
	if withThread.ThreadID.Native != "10/1" {
		t.Errorf("thread id = %q, want the thread zoho named", withThread.ThreadID.Native)
	}

	// Without one — which is every message on the listing and search endpoints — the
	// message's own id stands in, and the derived_threads quirk is what warns about it.
	withoutThread := p.convert(message{MessageID: "2", FolderID: "10"})
	if withoutThread.ThreadID.Native != "10/2" {
		t.Errorf("thread id = %q, want the message's own id as the fallback", withoutThread.ThreadID.Native)
	}
}

// Search reads the listing shape, where the same identifiers are quoted. Both spellings have
// to work through one decoder, or fixing the fetch would break the listing.
func TestZohoSearchReadsTheQuotedIdentifiers(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelopeOf(`[` + listingJSON + `]`)))
	})

	page, err := p.Search(context.Background(), mmail.Query{Limit: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want one message, got %d", len(page.Items))
	}
	if page.Items[0].ID.Native != liveFolderID+"/"+liveMessageID {
		t.Errorf("id = %q, want folder and message as the listing spelled them", page.Items[0].ID.Native)
	}
	if want := time.UnixMilli(1700000000000).UTC(); !page.Items[0].Date.Equal(want) {
		t.Errorf("date = %s, want %s", page.Items[0].Date, want)
	}
}
