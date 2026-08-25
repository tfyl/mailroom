package zoho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"golang.org/x/oauth2"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// testProvider points a provider at a stub Zoho, so the requests it builds can be inspected
// without credentials. Zoho has never run against a live mailbox; what these tests establish
// is what mailroom sends and what it claims, not what Zoho answers.
func testProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Provider{
		http:      srv.Client(),
		base:      srv.URL + "/api",
		accountID: "9000",
		account:   mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"status": map[string]any{"code": 200, "description": "success"},
		"data":   json.RawMessage(body),
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatal(err)
	}
}

// The no_batch quirk told callers that batches are looped one message at a time. They are
// not: updatemessage carries every id in one request, and a client that believed the quirk
// split work it could have sent whole.
func TestZohoBatchesEveryMessageIntoOneRequest(t *testing.T) {
	var requests int
	var sent []string

	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			MessageID []string `json:"messageId"`
			Mode      string   `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request body: %v", err)
		}
		sent = append(sent, body.MessageID...)
		writeEnvelope(t, w, map[string]any{})
	})

	ids := []mmail.ScopedID{
		{Account: "acct_1", Native: "10/1"},
		{Account: "acct_1", Native: "10/2"},
		{Account: "acct_1", Native: "11/3"},
	}
	if err := p.ApplyLabels(context.Background(), ids, []mmail.LabelID{"label:7"}, nil); err != nil {
		t.Fatalf("applying a label: %v", err)
	}

	if requests != 1 {
		t.Errorf("three messages and one label change should be one request, got %d", requests)
	}
	if !slices.Equal(sent, []string{"1", "2", "3"}) {
		t.Errorf("every message id should travel in that request, got %v", sent)
	}
}

func TestZohoDoesNotClaimBatchesAreLooped(t *testing.T) {
	for _, q := range (&Provider{}).Quirks() {
		if q == mmail.QuirkNoBatch {
			t.Error("zoho sends a batch in one request, so declaring no_batch costs throughput for nothing")
		}
	}
}

// A starred filter used to be dropped on the floor: the results came back unfiltered and
// nothing said so, which is the worst of the three possible answers.
func TestZohoSearchesForStarredWithTheFollowUpFlag(t *testing.T) {
	var query url.Values
	var path string

	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.Query()
		writeEnvelope(t, w, []message{})
	})

	if _, err := p.Search(context.Background(), mmail.Query{Starred: true}, ""); err != nil {
		t.Fatalf("searching for starred mail: %v", err)
	}
	if got := query.Get("flagid"); got != "3" {
		t.Errorf("starred maps onto Zoho's follow-up flag, so flagid should be 3, got %q", got)
	}
	if path != "/api/accounts/9000/messages/view" {
		t.Errorf("a filter with no search terms belongs on the listing endpoint, got %q", path)
	}
}

// Zoho's search syntax can ask only for flagged mail, which is a different question from
// "starred". Answering it anyway would return mail flagged some other way and call it
// starred, so the half of the query that cannot be served is refused by name.
func TestZohoRefusesAStarredSearchItCannotExpress(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the provider should have refused before calling zoho: %s", r.URL)
		writeEnvelope(t, w, []message{})
	})

	_, err := p.Search(context.Background(), mmail.Query{Starred: true, From: "alice@example.com"}, "")
	if err == nil {
		t.Fatal("a filter that cannot be honoured must be refused rather than ignored")
	}
	if code := mmail.Code(err); code != "unsupported_by_provider" {
		t.Errorf("want unsupported_by_provider so a client stops rather than retries, got %q", code)
	}
}

// The three sides of the mapping have to agree. SetFlags writes the follow-up flag for
// starred and Search filters on it, so a message carrying it has to read back as starred —
// otherwise a starred search answers with mail that says it is not starred.
func TestZohoReportsTheFollowUpFlagAsStarred(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work"}}

	starred := p.convert(message{MessageID: "1", FolderID: "10", FlagID: json.RawMessage(`"3"`)})
	if !starred.Flags.Starred {
		t.Error("a message carrying the follow-up flag should report as starred")
	}

	other := p.convert(message{MessageID: "2", FolderID: "10", FlagID: json.RawMessage(`"2"`)})
	if other.Flags.Starred {
		t.Error("the important flag is not the follow-up flag and must not read as starred")
	}
}

// Zoho answers its own token endpoint with `"token_type": "Bearer"` and then refuses that
// scheme on every Mail API endpoint. The header it wants is its own, so the standard OAuth
// client cannot be used: it takes the scheme from the token type.
func TestZohoAsksWithItsOwnAuthorizationScheme(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeEnvelope(t, w, []map[string]any{{"accountId": "9000", "primaryEmailAddress": "work@example.com"}})
	}))
	defer srv.Close()

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, srv.Client())
	p := &Provider{
		http: newClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "at", TokenType: "Bearer"})),
		base: srv.URL + "/api",
	}
	if _, err := p.discoverAccountID(ctx); err != nil {
		t.Fatal(err)
	}

	if got != "Zoho-oauthtoken at" {
		t.Errorf("Authorization = %q, want the Zoho-oauthtoken scheme", got)
	}
}
