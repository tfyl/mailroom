package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// draftsFolderListing is the answer every test here gives to /folders.
//
// The decoy comes first on purpose. A person may have a folder of their own called Drafts,
// and resolving to it would hand back an id that addresses a real folder and the wrong one —
// which is worse than a lookup that fails, because everything downstream succeeds.
func draftsFolderListing() map[string]any {
	return map[string]any{
		"status": map[string]any{"code": 200, "description": "success"},
		"data": []map[string]any{
			{"folderId": "101", "folderName": "Drafts", "isSystemFolder": false},
			{"folderId": "300", "folderName": "Drafts", "isSystemFolder": true},
			{"folderId": "222", "folderName": "Sent", "isSystemFolder": true},
		},
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("the request body did not decode: %v", err)
	}
	return body
}

// The draft endpoint is the send endpoint, and mode is the only thing that keeps them apart.
// A request that lost that field would not fail — it would deliver the mail — so this asserts
// on what actually reaches Zoho rather than on the id that comes back.
func TestADraftIsSavedRatherThanSent(t *testing.T) {
	var saved map[string]any
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			writeEnvelope(t, w, draftsFolderListing()["data"])
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		saved = decodeBody(t, r)
		// Zoho publishes no response body for a saved draft. This is the send's shape, which
		// was observed to carry a messageId and no folder at all.
		writeEnvelope(t, w, map[string]any{"messageId": "1234567890123456789"})
	})

	id, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Cc:      []mmail.Address{{Email: "other@example.com"}},
		Subject: "a draft",
		Body:    mmail.Body{Text: "not sent yet"},
	})
	if err != nil {
		t.Fatalf("saving a draft: %v", err)
	}

	if saved["mode"] != "draft" {
		t.Errorf("mode = %v, want draft — without it Zoho sends the message", saved["mode"])
	}
	if saved["toAddress"] != "someone@example.com" || saved["ccAddress"] != "other@example.com" {
		t.Errorf("recipients did not reach Zoho intact: %+v", saved)
	}
	if saved["subject"] != "a draft" || saved["content"] != "not sent yet" {
		t.Errorf("the message did not reach Zoho intact: %+v", saved)
	}

	// Zoho reported no folder, so the Drafts folder is resolved rather than invented — and it
	// is the system one, not the decoy the mailbox owner made.
	if id.Native != "300/1234567890123456789" {
		t.Fatalf("id = %q, want the system Drafts folder and the message id", id.Native)
	}
	if _, _, err := splitNative(id.Native); err != nil {
		t.Errorf("the id a draft returns must be one this provider accepts: %v", err)
	}
}

// Zoho answers a creation with an envelope code of 201, and reading anything but 200 as a
// failure is a bug this provider has already shipped once: CreateLabel returned an error
// having created the label, leaving one behind on every call. A draft saved and reported as
// failed would leave the same litter, and the caller would write it again.
func TestASavedDraftIsAcceptedUnderA201Envelope(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 201, "description": "Created"},
			"data":   map[string]any{"messageId": "1234567890123456789", "folderId": "300"},
		})
	})

	id, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account: "acct_1", To: []mmail.Address{{Email: "someone@example.com"}},
		Body: mmail.Body{Text: "hello"},
	})
	if err != nil {
		t.Fatalf("a 201 envelope is success, got: %v", err)
	}
	if id.Native != "300/1234567890123456789" {
		t.Errorf("id = %q, want the folder Zoho reported and the message id", id.Native)
	}
}

// The id is the whole product of this call. An answer without one has to be refused here,
// where the error can say what happened, rather than becoming "300/" and being rejected
// later at whatever the caller tried to do with it.
func TestADraftWithNoIDIsRefused(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, map[string]any{"folderId": "300"})
	})

	id, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account: "acct_1", To: []mmail.Address{{Email: "someone@example.com"}},
		Body: mmail.Body{Text: "hello"},
	})
	if err == nil {
		t.Fatalf("want an error, got the id %q", id.Native)
	}
	if !strings.Contains(err.Error(), "message id") {
		t.Errorf("the error must say what was missing: %v", err)
	}
	if !id.Zero() {
		t.Errorf("an id must not be handed back alongside the error: %q", id.Native)
	}
}

// A draft with attachments used to be refused here, before Zoho's upload endpoint was
// implemented. What replaced that refusal is in upload_test.go: the files go up first, and a
// draft whose files could not be stored is not saved either.

// Zoho has no call that rewrites a stored draft. The workaround — save a second draft and
// delete the first — is what this test exists to prevent somebody quietly adding: UpdateDraft
// reports no id, so the caller would keep one addressing a deleted message while its
// replacement sat in Drafts under an id nobody has.
//
// The assertion is therefore on the mailbox being left alone, not just on the error.
func TestEditingASavedDraftIsRefusedWithoutTouchingTheMailbox(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made: %s %s", r.Method, r.URL.Path)
	})

	err := p.UpdateDraft(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "300/999"},
		mmail.Outgoing{Account: "acct_1", Body: mmail.Body{Text: "revised"}})

	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want an unsupported_by_provider error, got %v", err)
	}
	if mmail.Code(err) != "unsupported_by_provider" {
		t.Errorf("code = %q, want unsupported_by_provider so a client can tell this apart from a failure",
			mmail.Code(err))
	}
}

// Zoho has no send-this-draft call either, and the workaround here is the dangerous one:
// rebuilding the draft as a fresh send drops its blind copies, because Zoho's metadata
// endpoint reports no bcc field of any spelling. The message would go to fewer people than it
// was addressed to and the result would say it went.
//
// So the property under test is that nothing goes out at all — no POST, no send.
func TestSendingASavedDraftIsRefusedWithoutSendingAnything(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should reach Zoho: %s %s", r.Method, r.URL.Path)
	})

	sent, err := p.SendDraft(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "300/999"})
	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want an unsupported_by_provider error, got %v", err)
	}
	if !sent.Zero() {
		t.Errorf("nothing was sent, so there is no id to report: %q", sent.Native)
	}
	if unsupported.Capability != mmail.CapSend {
		t.Errorf("capability = %q, want send: sending a saved draft is a send", unsupported.Capability)
	}
}

// Discarding a draft must not destroy mail. Zoho's delete takes an expunge flag that skips
// the bin, and Delete is the method that sends it — sending expunge here would be the
// destructive capability arriving through the discard door, on a grant that was never given
// it.
func TestDiscardingADraftDoesNotDestroyIt(t *testing.T) {
	var method, path string
	var query url.Values
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.Query()
		writeEnvelope(t, w, map[string]any{"cId": 1234567890123456789})
	})

	if err := p.DeleteDraft(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "300/999"}); err != nil {
		t.Fatalf("discarding a draft: %v", err)
	}

	if method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", method)
	}
	if !strings.HasSuffix(path, "/accounts/acct/folders/300/messages/999") {
		t.Errorf("path = %q, want the draft addressed by its own folder and id", path)
	}
	if query.Has("expunge") {
		t.Errorf("expunge = %q: discarding a draft must leave it recoverable in Trash",
			query.Get("expunge"))
	}
}

// A Zoho endpoint can answer HTTP 200 with a failing envelope inside it, and a delete is read
// for that envelope only because it is given somewhere to decode into. Without that, a
// discard that failed would be reported as done and the draft would still be in the mailbox.
func TestADiscardThatFailedInTheEnvelopeIsNotASuccess(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 500, "description": "Internal Error"},
		})
	})

	if err := p.DeleteDraft(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "300/999"}); err == nil {
		t.Fatal("a failing envelope under HTTP 200 must not be read as a discard")
	}
}

// An id with no folder cannot address a message on Zoho, and a delete built from one would
// be a request to a path that means something else. It is refused before anything is sent.
func TestDiscardingRefusesAnIDWithNoFolder(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made: %s %s", r.Method, r.URL.Path)
	})

	if err := p.DeleteDraft(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "999"}); err == nil {
		t.Fatal("a native id without a folder must be refused")
	}
}

// Zoho has no drafts listing; it has a folder listing that takes a folderId. The parameter
// this test cares about is that id, and that it is the system folder rather than a folder the
// mailbox owner happened to call Drafts — listing theirs would answer with mail that is not
// drafts at all, and every id in the page would then be offered for discarding.
func TestListingDraftsReadsTheSystemDraftsFolder(t *testing.T) {
	var query url.Values
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			writeEnvelope(t, w, draftsFolderListing()["data"])
			return
		}
		query = r.URL.Query()
		writeEnvelope(t, w, []map[string]any{{
			"messageId":   "1234567890123456789",
			"folderId":    "300",
			"subject":     "half a thought",
			"toAddress":   "someone@example.com",
			"fromAddress": "work@example.com",
			"status":      "1",
		}})
	})

	page, err := p.ListDrafts(context.Background(), "")
	if err != nil {
		t.Fatalf("listing drafts: %v", err)
	}

	if query.Get("folderId") != "300" {
		t.Errorf("folderId = %q, want the system Drafts folder", query.Get("folderId"))
	}
	if query.Get("start") != "1" {
		t.Errorf("start = %q: Zoho's listing is 1-indexed", query.Get("start"))
	}
	if query.Get("limit") == "" || query.Get("includeto") != "true" {
		t.Errorf("the listing must ask for a bounded page with recipients: %v", query)
	}

	if len(page.Items) != 1 {
		t.Fatalf("want one draft, got %d", len(page.Items))
	}
	draft := page.Items[0]
	if draft.ID.Native != "300/1234567890123456789" {
		t.Errorf("id = %q, want <folder>/<message>", draft.ID.Native)
	}
	// Zoho reports no draft flag anywhere in the listing; the folder is what makes these
	// drafts, and a caller that is not told may treat one as mail it can reply to.
	if !draft.Flags.Draft {
		t.Error("a message listed out of the Drafts folder must be flagged as a draft")
	}
	// A short page is the end of the list: Zoho reports no total, so a cursor here would send
	// the caller after a page that does not exist.
	if page.Cursor != "" {
		t.Errorf("cursor = %q, want none on a short page", page.Cursor)
	}
}

// The cursor is an offset, and the two halves have to agree: a full page hands one out, and
// handing it back has to resume where that page stopped rather than at the top.
func TestListingDraftsPagesFromTheCursorItHandedOut(t *testing.T) {
	full := make([]map[string]any, 0, defaultPageSize)
	for i := 0; i < defaultPageSize; i++ {
		full = append(full, map[string]any{
			"messageId": fmt.Sprintf("10000000000000000%02d", i),
			"folderId":  "300",
			"subject":   "half a thought",
		})
	}

	var starts []string
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			writeEnvelope(t, w, draftsFolderListing()["data"])
			return
		}
		starts = append(starts, r.URL.Query().Get("start"))
		writeEnvelope(t, w, full)
	})

	page, err := p.ListDrafts(context.Background(), "")
	if err != nil {
		t.Fatalf("listing drafts: %v", err)
	}
	want := fmt.Sprint(1 + defaultPageSize)
	if page.Cursor != want {
		t.Fatalf("cursor = %q, want %q", page.Cursor, want)
	}

	if _, err := p.ListDrafts(context.Background(), page.Cursor); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if len(starts) != 2 || starts[0] != "1" || starts[1] != want {
		t.Errorf("start values were %v, want [1 %s]", starts, want)
	}
}

// A cursor this provider did not produce is refused rather than silently read as the top of
// the list, which would hand the caller the first page again and look like the walk had
// looped.
func TestListingDraftsRefusesACursorItDidNotProduce(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made for a cursor that cannot be read: %s", r.URL.Path)
	})

	if _, err := p.ListDrafts(context.Background(), "page-two"); err == nil {
		t.Fatal("a malformed cursor must be refused")
	}
}

// Measured against the live mailbox: posting mode=draft alongside the reply fields Send uses
// comes back 404 EXTRA_KEY_FOUND_IN_JSON, which this provider maps to not_found — so a caller
// asking to draft a reply was told its message did not exist. Refused by name instead, and
// deliberately not saved as an ordinary draft: that would sit in Drafts detached from the
// conversation it answers with nothing having said so.
func TestAReplyDraftIsRefusedRatherThanSavedDetached(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 200},
			"data":   map[string]any{"messageId": "1", "folderId": "2"},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}

	_, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account:   "acct_1",
		InReplyTo: mmail.ScopedID{Account: "acct_1", Native: "300/400"},
		To:        []mmail.Address{{Email: "someone@example.com"}},
		Subject:   "Re: hello",
		Body:      mmail.Body{Text: "answering"},
	})

	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want an unsupported refusal, got %v", err)
	}
	if posted {
		t.Error("nothing should have been sent to Zoho; a saved-but-detached draft is the thing being avoided")
	}
	if !strings.Contains(unsupported.Reason, "detached") {
		t.Errorf("the refusal should say why saving it anyway is wrong: %s", unsupported.Reason)
	}
}
