package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// testProvider points a provider at a stub Graph, so the requests it builds can be inspected
// without credentials. What these tests establish is what mailroom sends and what it claims.
// Graph has since answered a real mailbox, and where a case below is shaped by what it
// actually returned rather than by what its documentation says, the test says so.
func testProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Provider{
		http:    srv.Client(),
		base:    srv.URL + "/v1.0",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

// Every request has to ask for immutable ids, and asking on some requests and not others is
// worse than never asking: an id minted in one mode does not resolve in the other, so a
// single request that forgot the header hands back an id that fails everywhere it is used.
func TestEveryRequestAsksForImmutableIDs(t *testing.T) {
	seen := map[string]string{}

	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Prefer")
		switch {
		case strings.HasSuffix(r.URL.Path, "/attachments"):
			writeJSON(t, w, map[string]any{"value": []attachmentRef{}})
		case strings.Contains(r.URL.Path, "/messages/"):
			writeJSON(t, w, message{ID: "m1", HasAttachments: true})
		default:
			writeJSON(t, w, messagePage{Value: []message{{ID: "m1"}}})
		}
	})

	ctx := context.Background()
	if _, err := p.Search(ctx, mmail.Query{}, ""); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, err := p.Get(ctx, mmail.ScopedID{Account: "acct_1", Native: "m1"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := p.SetFlags(ctx, []mmail.ScopedID{{Account: "acct_1", Native: "m1"}}, mmail.FlagUpdate{Read: ptr(true)}); err != nil {
		t.Fatalf("set flags: %v", err)
	}

	if len(seen) < 3 {
		t.Fatalf("expected several requests to inspect, got %v", seen)
	}
	for request, prefer := range seen {
		if prefer != preferImmutableIDs {
			t.Errorf("%s carried Prefer %q, want %q — an id minted without it stops resolving "+
				"the moment the message moves", request, prefer, preferImmutableIDs)
		}
	}
}

// A 401 has to arrive as ErrNeedsReauth, because that is the outcome the grant gate watches
// for: it marks the mailbox as needing re-linking, and anything else leaves a client retrying
// forever against credentials that will never work again.
func TestGraphRefusalSurfacesAsAuthExpired(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, map[string]any{"error": map[string]any{
			"code":    "InvalidAuthenticationToken",
			"message": "Access token has expired or is not yet valid.",
		}})
	})

	_, err := p.Search(context.Background(), mmail.Query{}, "")
	if err == nil {
		t.Fatal("a 401 must be reported as a failure")
	}
	if code := mmail.Code(err); code != mmail.CodeAuthExpired {
		t.Errorf("want %q so the mailbox is marked as needing re-linking, got %q: %v",
			mmail.CodeAuthExpired, code, err)
	}
}

// A missing message and a transport failure must be distinguishable, and Graph has three ways
// of saying missing.
//
// The last case is the one a caller actually hits, and it is the one this originally got
// wrong: Graph parses an id before looking it up, so anything not well-formed — a stale id, an
// invented one, a truncated one — is a 400 ErrorInvalidIdMalformed and never reaches either
// not-found code. Live conformance found it; the stub above had only ever been told to answer
// with ErrorItemNotFound.
func TestAMissingMessageIsReportedAsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		msg    string
	}{
		{"not in the store", http.StatusNotFound, "ErrorItemNotFound", "The specified object was not found in the store."},
		{"id no longer addresses anything", http.StatusBadRequest, "ErrorItemNotFound", "The specified object was not found in the store."},
		{"id Graph will not parse", http.StatusBadRequest, "ErrorInvalidIdMalformed", "Id is malformed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				writeJSON(t, w, map[string]any{"error": map[string]any{
					"code": tc.code, "message": tc.msg,
				}})
			})

			_, err := p.Get(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "gone"})
			if code := mmail.Code(err); code != "not_found" {
				t.Errorf("want not_found, got %q: %v", code, err)
			}
		})
	}
}

// A throttle is retryable and says when. Reporting it as a permanent failure would have a
// client give up on a mailbox that is merely busy.
func TestAThrottleIsRetryableAndCarriesTheWait(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(t, w, map[string]any{"error": map[string]any{"code": "ApplicationThrottled"}})
	})

	_, err := p.Search(context.Background(), mmail.Query{}, "")
	if !mmail.Retryable(err) {
		t.Fatalf("a throttle must be retryable, got %v", err)
	}
	var provider *mmail.ProviderError
	if !asProviderError(err, &provider) {
		t.Fatalf("want a ProviderError, got %T", err)
	}
	if provider.RetryIn != 17 {
		t.Errorf("RetryIn = %d, want the 17 seconds Graph asked for", provider.RetryIn)
	}
}

func asProviderError(err error, target **mmail.ProviderError) bool {
	if p, ok := err.(*mmail.ProviderError); ok {
		*target = p
		return true
	}
	return false
}

// A query with no free-text terms belongs on $filter. Putting it on $search instead would
// answer a fuzzier question and lose any ordering.
func TestStructuredQueriesUseFilter(t *testing.T) {
	var query url.Values
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(t, w, messagePage{})
	})

	_, err := p.Search(context.Background(), mmail.Query{Unread: true, Starred: true, HasAttach: true}, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	filter := query.Get("$filter")
	for _, want := range []string{"isRead eq false", "flag/flagStatus eq 'flagged'", "hasAttachments eq true"} {
		if !strings.Contains(filter, want) {
			t.Errorf("$filter = %q, missing %q", filter, want)
		}
	}
	if query.Get("$search") != "" {
		t.Errorf("a structured query must not go to $search, got %q", query.Get("$search"))
	}
}

// Exchange refuses a sort on a property the filter does not lead with — InefficientFilter,
// "the restriction or sort order is too complex for this operation". So the date clauses are
// written first and the sort is asked for only where that makes it legal. Getting this wrong
// does not degrade the ordering; it fails the whole search.
func TestOrderingIsAskedForOnlyWhereExchangeWillServeIt(t *testing.T) {
	var query url.Values
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(t, w, messagePage{})
	})
	search := func(q mmail.Query) {
		t.Helper()
		if _, err := p.Search(context.Background(), q, ""); err != nil {
			t.Fatalf("search: %v", err)
		}
	}

	search(mmail.Query{})
	if query.Get("$orderby") != "receivedDateTime desc" {
		t.Errorf("an unfiltered listing may be ordered; $orderby = %q", query.Get("$orderby"))
	}

	search(mmail.Query{After: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Unread: true})
	if query.Get("$orderby") != "receivedDateTime desc" {
		t.Errorf("a filter led by the date may be ordered by it; $orderby = %q", query.Get("$orderby"))
	}
	if !strings.HasPrefix(query.Get("$filter"), "receivedDateTime ge") {
		t.Errorf("the sorted property must come first in the filter, got %q", query.Get("$filter"))
	}

	search(mmail.Query{Unread: true})
	if query.Get("$orderby") != "" {
		t.Errorf("a filter that never mentions the date cannot be sorted on it, and asking "+
			"fails the search outright; $orderby = %q", query.Get("$orderby"))
	}
}

// Free-text terms go to $search, and $search on messages cannot be ordered or filtered
// alongside — so nothing else may ride with it.
func TestFreeTextQueriesUseSearchAlone(t *testing.T) {
	var query url.Values
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(t, w, messagePage{})
	})

	_, err := p.Search(context.Background(), mmail.Query{Subject: "quarterly report", From: "ada@example.com"}, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	search := query.Get("$search")
	for _, want := range []string{`subject:"quarterly report"`, "from:ada@example.com"} {
		if !strings.Contains(search, want) {
			t.Errorf("$search = %q, missing %q", search, want)
		}
	}
	if query.Get("$filter") != "" || query.Get("$orderby") != "" {
		t.Errorf("Graph evaluates $search on messages without $filter or $orderby; got filter=%q orderby=%q",
			query.Get("$filter"), query.Get("$orderby"))
	}
}

// Half a query silently dropped is the worst of the three possible answers: the caller gets
// results that look like an answer to what it asked.
func TestSearchRefusesAFilterItCannotExpressAlongsideTerms(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider should have refused before calling Graph: %s", r.URL)
		writeJSON(t, w, messagePage{})
	})

	_, err := p.Search(context.Background(), mmail.Query{Raw: "invoice", Unread: true}, "")
	if err == nil {
		t.Fatal("a filter that cannot be honoured must be refused rather than ignored")
	}
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider so a client stops rather than retries, got %q", code)
	}
}

// Paging follows the link Graph hands back, which is what the documentation says to do with
// it. A cursor that names any other host is refused: a cursor travels out to a client and
// comes back, so this is the one place a caller could otherwise choose where this process
// sends its credentials.
func TestPagingFollowsTheNextLinkAndRefusesAForeignOne(t *testing.T) {
	var paths []string
	var p *Provider
	p = testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Query().Get("$skiptoken") == "" {
			writeJSON(t, w, messagePage{
				Value:    []message{{ID: "m1"}},
				NextLink: p.base + "/me/messages?$skiptoken=abc",
			})
			return
		}
		writeJSON(t, w, messagePage{Value: []message{{ID: "m2"}}})
	})

	ctx := context.Background()
	first, err := p.Search(ctx, mmail.Query{}, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Cursor == "" {
		t.Fatal("a page with a nextLink must report a cursor")
	}

	second, err := p.Search(ctx, mmail.Query{}, first.Cursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID.Native != "m2" {
		t.Errorf("the cursor did not fetch the next page, got %+v", second.Items)
	}
	if second.Cursor != "" {
		t.Error("a page with no nextLink must end the walk")
	}
	if len(paths) != 2 {
		t.Errorf("want one request per page, got %v", paths)
	}

	if _, err := p.Search(ctx, mmail.Query{}, "https://elsewhere.example.com/v1.0/me/messages"); err == nil {
		t.Error("a cursor naming another host must be refused, not fetched")
	}
}

// The three sides of the starred mapping have to agree. SetFlags writes the follow-up flag,
// Search filters on it, and a message carrying it has to read back as starred — otherwise a
// starred search answers with mail that then says it is not starred.
func TestTheFollowUpFlagIsStarredInEveryDirection(t *testing.T) {
	var written map[string]any
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
			t.Errorf("decoding the patch: %v", err)
		}
		writeJSON(t, w, message{ID: "m1"})
	})

	err := p.SetFlags(context.Background(), []mmail.ScopedID{{Account: "acct_1", Native: "m1"}},
		mmail.FlagUpdate{Starred: ptr(true)})
	if err != nil {
		t.Fatalf("set flags: %v", err)
	}
	flag, _ := written["flag"].(map[string]any)
	if flag["flagStatus"] != flagged {
		t.Errorf("starred must write the follow-up flag, got %v", written["flag"])
	}

	starred := p.convert(message{ID: "m1", Flag: &followupFlag{FlagStatus: flagged}})
	if !starred.Flags.Starred {
		t.Error("a flagged message must read back as starred")
	}
	complete := p.convert(message{ID: "m2", Flag: &followupFlag{FlagStatus: "complete"}})
	if complete.Flags.Starred {
		t.Error("a flag marked complete is no longer a star and must not read as one")
	}
}

// Categories are a whole-array property. Patching only what the caller asked for would drop
// every category the message already carried, which is a silent loss of somebody's filing.
func TestApplyingACategoryKeepsTheOnesAlreadyThere(t *testing.T) {
	var patched []string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, message{ID: "m1", Categories: []string{"Blue category", "Red category"}})
			return
		}
		var body struct {
			Categories []string `json:"categories"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the patch: %v", err)
		}
		patched = body.Categories
		writeJSON(t, w, message{ID: "m1"})
	})

	ids := []mmail.ScopedID{{Account: "acct_1", Native: "m1"}}
	err := p.ApplyLabels(context.Background(), ids,
		[]mmail.LabelID{"category:Green category"}, []mmail.LabelID{"category:Red category"})
	if err != nil {
		t.Fatalf("apply labels: %v", err)
	}

	want := map[string]bool{"Blue category": true, "Green category": true}
	if len(patched) != len(want) {
		t.Fatalf("categories = %v, want exactly %v", patched, want)
	}
	for _, c := range patched {
		if !want[c] {
			t.Errorf("categories = %v, %q should not be among them", patched, c)
		}
	}
}

// Sending goes through a draft so there is an id to report. sendMail answers with nothing at
// all, which leaves a caller unable to look at what it just sent.
func TestSendCreatesADraftAndReportsItsID(t *testing.T) {
	var calls []string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		writeJSON(t, w, message{ID: "draft-1"})
	})

	id, err := p.Send(context.Background(), mmail.Outgoing{
		To: []mmail.Address{{Email: "ada@example.com"}}, Subject: "hello",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id.Native != "draft-1" || id.Account != "acct_1" {
		t.Errorf("send reported %s, want the draft's own id stamped with this account", id)
	}
	want := []string{"POST /v1.0/me/messages", "POST /v1.0/me/messages/draft-1/send"}
	if strings.Join(calls, " | ") != strings.Join(want, " | ") {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

// An Outlook automatic reply has no subject of its own. Accepting one would mean quietly not
// doing what was asked, on a setting nobody looks at again until somebody outside mentions
// the reply they got.
func TestSettingAVacationSubjectIsRefusedRatherThanDropped(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider should have refused before calling Graph: %s", r.URL)
		writeJSON(t, w, map[string]any{})
	})

	err := p.SetVacation(context.Background(), mmail.Vacation{
		Enabled: true, Subject: "Out of office", Body: "Back on Monday.",
	})
	if err == nil {
		t.Fatal("a subject that cannot be honoured must be refused")
	}
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider, got %q: %v", code, err)
	}
}

// Exchange refuses message rules outright on a personal Microsoft account, with a 403 that
// otherwise reads as a permission problem an operator would go looking for a scope to fix.
// There is no scope: the feature does not exist there.
func TestAConsumerMailboxRefusingRulesIsReportedAsUnsupported(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, map[string]any{"error": map[string]any{
			"code": "ErrorAccessDenied", "message": "Access is denied.",
		}})
	})

	_, err := p.ListFilters(context.Background())
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider so a caller stops rather than retries, got %q: %v", code, err)
	}
	var unsupported *mmail.UnsupportedError
	if u, ok := err.(*mmail.UnsupportedError); ok {
		unsupported = u
	}
	if unsupported == nil {
		t.Fatalf("want an UnsupportedError, got %T", err)
	}
	if unsupported.Capability != mmail.CapFilters {
		t.Errorf("the refusal names %q; rules are the filters capability", unsupported.Capability)
	}
	if unsupported.Op == "" {
		t.Error("the refusal must name the operation, since the neighbouring settings calls work")
	}
}

// A rule can add a category and move mail; it has no action that takes either away. Writing
// half of what was asked for and saying nothing is what this refuses to do.
func TestCreatingAFilterThatRemovesALabelIsRefused(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider should have refused before calling Graph: %s", r.URL)
		writeJSON(t, w, map[string]any{})
	})

	_, err := p.CreateFilter(context.Background(), mmail.Filter{
		From: "alerts@example.com", RemoveLabels: []mmail.LabelID{"category:Red category"},
	})
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider, got %q: %v", code, err)
	}
}

// An id from Graph is not URL-safe. Skipping the escaping produces a request that addresses
// something else, or nothing.
func TestMessageIDsAreEscapedIntoThePath(t *testing.T) {
	const native = "AAkALgAAA/AAA=+id"

	var got string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		writeJSON(t, w, message{ID: native})
	})

	if _, err := p.Get(context.Background(), mmail.ScopedID{Account: "acct_1", Native: native}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := "/v1.0/me/messages/" + url.PathEscape(native); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// The address a mailbox is linked under comes from Microsoft rather than from anything typed
// into a form, and an account with no Exchange mail property still has a name worth using.
func TestTheLinkedAddressFallsBackToTheUserPrincipalName(t *testing.T) {
	if got := (meResponse{Mail: "ada@example.com", UserPrincipalName: "ada@tenant.onmicrosoft.com"}).address(); got != "ada@example.com" {
		t.Errorf("the mail property wins when there is one, got %q", got)
	}
	if got := (meResponse{UserPrincipalName: "ada@tenant.onmicrosoft.com"}).address(); got != "ada@tenant.onmicrosoft.com" {
		t.Errorf("want the principal name when there is no mail property, got %q", got)
	}
	if got := (meResponse{UserPrincipalName: "not-an-address"}).address(); got != "" {
		t.Errorf("a principal name that is not an address is no address at all, got %q", got)
	}
}

// Microsoft documents no filterable-property list for messages and publishes not one example
// of filtering on conversationId, while Graph's known-issues page says an unsupported filter
// can be dropped in silence. A silently ignored filter here returns the whole mailbox as the
// conversation, so what comes back is checked rather than trusted.
func TestAThreadDropsWhateverTheFilterDidNot(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$filter"); !strings.Contains(got, "conversationId eq 'conv-1'") {
			t.Errorf("$filter = %q, want the conversation", got)
		}
		// A Graph that ignored the filter, which is the failure this guards against.
		writeJSON(t, w, messagePage{Value: []message{
			{ID: "m1", ConversationID: "conv-1", ReceivedDateTime: "2026-08-02T10:00:00Z"},
			{ID: "m2", ConversationID: "conv-1", ReceivedDateTime: "2026-08-01T10:00:00Z"},
			{ID: "unrelated", ConversationID: "conv-9", ReceivedDateTime: "2026-08-03T10:00:00Z"},
		}})
	})

	thread, err := p.GetThread(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "conv-1"})
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("want only the conversation's own messages, got %d: %+v", len(thread.Messages), thread.Messages)
	}
	// Oldest first, sorted here because Exchange refuses a sort on a property the filter is
	// not led by.
	if thread.Messages[0].ID.Native != "m2" {
		t.Errorf("a thread reads oldest first, got %s", thread.Messages[0].ID)
	}
	if thread.Derived {
		t.Error("Exchange assigns the conversation, so the grouping is not derived")
	}
}

// A rule that files mail into Deleted Items has to come back out of ListFilters as the same
// rule that went in. Graph documents moveToFolder as taking a folder id, so the well-known
// name goes to the delete action instead — and the read maps it back.
func TestAFilterThatDeletesRoundTrips(t *testing.T) {
	var written messageRule
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, map[string]any{"value": []messageRule{}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
			t.Errorf("decoding the rule: %v", err)
		}
		written.ID = "rule-1"
		writeJSON(t, w, written)
	})

	created, err := p.CreateFilter(context.Background(), mmail.Filter{
		From: "noreply@example.com", AddLabels: []mmail.LabelID{folderLabel(deletedItems)},
	})
	if err != nil {
		t.Fatalf("create filter: %v", err)
	}
	if written.Actions == nil || !written.Actions.Delete {
		t.Errorf("filing into Deleted Items is the rule's delete action, got %+v", written.Actions)
	}
	if written.Actions != nil && written.Actions.MoveToFolder != "" {
		t.Errorf("moveToFolder takes a folder id, not a well-known name; got %q", written.Actions.MoveToFolder)
	}
	if len(created.AddLabels) != 1 || created.AddLabels[0] != folderLabel(deletedItems) {
		t.Errorf("the rule did not round trip: %+v", created.AddLabels)
	}
	if written.DisplayName == "" {
		t.Error("Exchange requires a rule to be named")
	}
	if written.Sequence < 1 {
		t.Errorf("a rule needs a position in the order; sequence = %d", written.Sequence)
	}
}

// Graph lists Content-Length as a required header on a send, and a request with no body is
// exactly where Go could reasonably omit it.
func TestSendingADraftCarriesAnExplicitZeroLength(t *testing.T) {
	var length string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/send") {
			length = r.Header.Get("Content-Length")
		}
		w.WriteHeader(http.StatusAccepted)
	})

	if _, err := p.SendDraft(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "m1"}); err != nil {
		t.Fatalf("send draft: %v", err)
	}
	if length != "0" {
		t.Errorf("Content-Length = %q, want the explicit 0 Graph asks for", length)
	}
}

// Exchange caps a single message at 500 recipients and rejects the send a long way from the
// count that caused it. Refusing here says which number was the problem.
func TestTooManyRecipientsIsRefusedBeforeSending(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider should have refused before calling Graph: %s", r.URL)
		writeJSON(t, w, message{ID: "m1"})
	})

	crowd := make([]mmail.Address, maxRecipients+1)
	for i := range crowd {
		crowd[i] = mmail.Address{Email: "someone@example.com"}
	}

	_, err := p.Send(context.Background(), mmail.Outgoing{To: crowd, Subject: "all hands"})
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider, got %q: %v", code, err)
	}
	if err != nil && !strings.Contains(err.Error(), "501") {
		t.Errorf("the refusal should name the count that caused it: %v", err)
	}
}

// ptr is the shorthand a FlagUpdate needs: its fields are pointers so that a field left nil
// means "leave this flag alone" rather than "set it to false".
func ptr[T any](v T) *T { return &v }

// A PATCH writes the properties named in it and leaves the rest alone, so a flag update that
// names one of the two must name only that one.
//
// The body used to carry both every time: marking a message read also wrote
// flag/flagStatus notFlagged, which clears somebody's follow-up flag on a request that said
// nothing about it and reports success.
func TestAFlagUpdateWritesOnlyWhatItNames(t *testing.T) {
	var written map[string]any
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
			t.Errorf("decoding the patch: %v", err)
		}
		writeJSON(t, w, message{ID: "m1"})
	})

	err := p.SetFlags(context.Background(), []mmail.ScopedID{{Account: "acct_1", Native: "m1"}},
		mmail.FlagUpdate{Read: ptr(true)})
	if err != nil {
		t.Fatalf("marking read: %v", err)
	}

	if written["isRead"] != true {
		t.Errorf("isRead = %v, want true", written["isRead"])
	}
	if _, present := written["flag"]; present {
		t.Errorf("the patch wrote the follow-up flag on a request that said nothing about "+
			"it: %v", written)
	}
}

// A filter Graph ignored must not come back as an answer.
//
// Microsoft publishes no filterable-property list for the message resource, and filtering on
// flag/flagStatus or on categories appears in no example anywhere; the known-issues page says
// an unsupported query parameter "might fail silently". So a starred search can come back as
// the whole mailbox, 200 and well formed, and every message in it presented as starred.
//
// GetThread has always defended conversationId this way. Search now does the same for the
// predicates that rest on nothing.
func TestSearchDropsResultsAFilterShouldHaveExcluded(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// What an ignored filter looks like: everything, regardless of what was asked.
		writeJSON(t, w, messagePage{Value: []message{
			{ID: "flagged", Flag: &followupFlag{FlagStatus: flagged}},
			{ID: "not-flagged", Flag: &followupFlag{FlagStatus: notFlagged}},
			{ID: "no-flag-at-all"},
		}})
	})

	page, err := p.Search(context.Background(), mmail.Query{Starred: true}, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("only the flagged message answers a starred search, got %d", len(page.Items))
	}
	if page.Items[0].ID.Native != "flagged" {
		t.Errorf("kept the wrong message: %s", page.Items[0].ID.Native)
	}
}

// The same defence has to survive paging. A nextLink carries the original query parameters,
// so a filter Graph ignored on the first page it ignores on every one after it.
func TestPagedResultsAreCheckedToo(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, messagePage{Value: []message{
			{ID: "has-one", HasAttachments: true},
			{ID: "has-none"},
		}})
	})

	page, err := p.Search(context.Background(), mmail.Query{HasAttach: true},
		p.base+"/me/messages?$skip=10")
	if err != nil {
		t.Fatalf("following the cursor: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID.Native != "has-one" {
		t.Errorf("the second page was not checked: %v", page.Items)
	}
}

// Graph refuses a null collection on POST /me/messages with UnableToDeserializePostBody, and
// an ordinary message has no cc and no bcc — so a nil slice here took out drafting and
// sending entirely against a real mailbox, while every stub-backed test passed.
//
// An empty array rather than an omitted property, because the same body is PATCHed to update
// a draft, where [] clears the recipients and omitting the key leaves them in place.
func TestEmptyRecipientCollectionsAreSentAsArraysNotNull(t *testing.T) {
	var got map[string]any
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding the request body: %v", err)
		}
		writeJSON(t, w, map[string]any{"id": "AAMk-created"})
	})

	if _, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Subject: "no cc, no bcc",
		Body:    mmail.Body{Text: "hello"},
	}); err != nil {
		t.Fatalf("creating a draft: %v", err)
	}

	for _, field := range []string{"ccRecipients", "bccRecipients"} {
		value, present := got[field]
		if !present {
			t.Errorf("%s was omitted; PATCH needs it present to clear recipients", field)
			continue
		}
		list, ok := value.([]any)
		if !ok {
			t.Errorf("%s = %#v, want an empty array — null is what Graph refuses", field, value)
			continue
		}
		if len(list) != 0 {
			t.Errorf("%s = %#v, want empty", field, list)
		}
	}
	if _, ok := got["toRecipients"].([]any); !ok {
		t.Errorf("toRecipients = %#v, want an array", got["toRecipients"])
	}
}
