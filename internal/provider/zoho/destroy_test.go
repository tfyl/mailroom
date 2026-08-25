package zoho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// binFolderListing is the answer every test here gives to /folders.
//
// Both decoys come first on purpose. A person may have folders of their own called Trash and
// Inbox, and resolving to one of those would hand back an id that addresses a real folder and
// the wrong one — so a Trash would file mail into somebody's own folder and report that it had
// been binned, which is worse than a lookup that fails because everything downstream succeeds.
func binFolderListing() []map[string]any {
	return []map[string]any{
		{"folderId": "101", "folderName": "Trash", "isSystemFolder": false},
		{"folderId": "102", "folderName": "Inbox", "isSystemFolder": false},
		{"folderId": "202", "folderName": "Trash", "isSystemFolder": true},
		{"folderId": "303", "folderName": "Inbox", "isSystemFolder": true},
		{"folderId": "404", "folderName": "Receipts"},
	}
}

// moveRecorder answers the folder listing and captures the update request, which is the one
// that decides where mail ends up.
type moveRecorder struct {
	updates int
	path    string
	method  string
	body    map[string]any
}

func (m *moveRecorder) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			writeEnvelope(t, w, binFolderListing())
			return
		}
		m.updates++
		m.method, m.path = r.Method, r.URL.Path
		m.body = decodeBody(t, r)
		// Zoho's documented answer to a move is an envelope with no data object at all.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 200, "description": "success"},
		})
	}
}

func (m *moveRecorder) messageIDs(t *testing.T) []string {
	t.Helper()
	raw, ok := m.body["messageId"].([]any)
	if !ok {
		t.Fatalf("messageId was not a JSON array: %#v", m.body["messageId"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// Trashing has to reach Zoho as a move into the system bin, in one request for the whole
// batch. The destination is the assertion that matters: a move that resolved to the mailbox
// owner's own folder called Trash would succeed, file the mail somewhere real, and report that
// it had been binned.
func TestTrashingMovesEveryMessageIntoTheSystemBinInOneRequest(t *testing.T) {
	var rec moveRecorder
	p := zohoProvider(t, rec.handler(t))

	ids := []mmail.ScopedID{
		{Account: "acct_1", Native: "303/1234567890123456789"},
		{Account: "acct_1", Native: "404/1234567890123456790"},
	}
	if err := p.Trash(context.Background(), ids); err != nil {
		t.Fatalf("trashing: %v", err)
	}

	if rec.updates != 1 {
		t.Errorf("two messages should be one request, got %d", rec.updates)
	}
	if rec.method != http.MethodPut || !strings.HasSuffix(rec.path, "/accounts/acct/updatemessage") {
		t.Errorf("request was %s %s, want PUT the update endpoint", rec.method, rec.path)
	}
	if rec.body["mode"] != "moveMessage" {
		t.Errorf("mode = %v, want moveMessage", rec.body["mode"])
	}
	if rec.body["destfolderId"] != "202" {
		t.Errorf("destfolderId = %v, want 202 — the system bin, not the folder the mailbox "+
			"owner called Trash", rec.body["destfolderId"])
	}
	if got := rec.messageIDs(t); !slices.Equal(got, []string{"1234567890123456789", "1234567890123456790"}) {
		t.Errorf("message ids reaching Zoho = %v, want both, without their folders", got)
	}
}

// Restoring is the same request with a different destination, and it has to be the system
// inbox for the same reason trashing has to be the system bin.
func TestUntrashingRestoresToTheSystemInbox(t *testing.T) {
	var rec moveRecorder
	p := zohoProvider(t, rec.handler(t))

	if err := p.Untrash(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "202/1234567890123456789"}}); err != nil {
		t.Fatalf("untrashing: %v", err)
	}

	if rec.body["mode"] != "moveMessage" {
		t.Errorf("mode = %v, want moveMessage", rec.body["mode"])
	}
	if rec.body["destfolderId"] != "303" {
		t.Errorf("destfolderId = %v, want 303 — the system inbox", rec.body["destfolderId"])
	}
}

// A move changes the folder half of an id without telling anybody, so after a Trash the id the
// caller still holds names the folder the message was in before. Untrash therefore must not
// read that half or check it: doing so would refuse exactly the round trip it would exist to
// protect. Zoho addresses a move by message id alone, which is what makes that possible.
//
// The assertion is that the stale folder neither reaches Zoho as a source hint nor stops the
// restore.
func TestUntrashingIgnoresTheFolderHalfOfAStaleID(t *testing.T) {
	var rec moveRecorder
	p := zohoProvider(t, rec.handler(t))

	// 404 is an ordinary folder: this is the id from before the message was trashed.
	if err := p.Untrash(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "404/1234567890123456789"}}); err != nil {
		t.Fatalf("a restore must not depend on where the id says the message is: %v", err)
	}

	if _, present := rec.body["folderId"]; present {
		t.Errorf("the source folder was sent as %v; the id's folder half is stale after a "+
			"move, so naming it would address the wrong place", rec.body["folderId"])
	}
	if _, present := rec.body["isFolderSpecific"]; present {
		t.Error("isFolderSpecific would tie the move to a folder half this provider cannot trust")
	}
	if got := rec.messageIDs(t); !slices.Equal(got, []string{"1234567890123456789"}) {
		t.Errorf("message ids reaching Zoho = %v, want the message restored regardless of folder", got)
	}
}

// An id with no folder cannot address a Zoho message, and a batch containing one is refused
// before anything is asked of Zoho — including the folder listing, which would otherwise be a
// request made on behalf of a call that was never going to happen.
func TestTrashingRefusesAMalformedIDWithoutTouchingTheMailbox(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made: %s %s", r.Method, r.URL.Path)
	})

	err := p.Trash(context.Background(), []mmail.ScopedID{
		{Account: "acct_1", Native: "303/1234567890123456789"},
		{Account: "acct_1", Native: "1234567890123456790"},
	})
	if err == nil {
		t.Fatal("a native id without a folder must be refused")
	}
}

// Trashing nothing is not a reason to talk to Zoho at all. Without the guard the folder
// listing goes out to resolve a destination for an empty batch.
func TestTrashingNothingAsksZohoNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made for an empty batch: %s %s", r.Method, r.URL.Path)
	})

	if err := p.Trash(context.Background(), nil); err != nil {
		t.Fatalf("trashing nothing: %v", err)
	}
	if err := p.Untrash(context.Background(), nil); err != nil {
		t.Fatalf("untrashing nothing: %v", err)
	}
}

// A Zoho endpoint can answer HTTP 200 with a failing envelope inside it, and that envelope is
// read only because the move is given somewhere to decode into. Without it, mail that never
// left the folder its owner wanted it out of would be reported as binned.
func TestATrashThatFailedInTheEnvelopeIsNotASuccess(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			writeEnvelope(t, w, binFolderListing())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 500, "description": "Internal Error"},
		})
	})

	if err := p.Trash(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "303/1234567890123456789"}}); err == nil {
		t.Fatal("a failing envelope under HTTP 200 must not be read as a trashing")
	}
}

// Delete is the one call here with no undo, so the flag that makes it permanent is the whole
// point of it. Without expunge Zoho moves the message to Trash and answers success, and a
// caller who asked for the mail to be destroyed would be told it had been.
func TestDeletingAsksZohoToDestroyRatherThanBin(t *testing.T) {
	var methods, paths []string
	var queries []url.Values
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.Query())
		writeEnvelope(t, w, map[string]any{"cId": 1234567890123456789})
	})

	ids := []mmail.ScopedID{
		{Account: "acct_1", Native: "202/1234567890123456789"},
		{Account: "acct_1", Native: "404/1234567890123456790"},
	}
	if err := p.Delete(context.Background(), ids); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	// Zoho publishes no bulk delete, so a batch is a request per message, each addressing the
	// message by its own folder rather than by whichever folder came first.
	wantPaths := []string{
		"/accounts/acct/folders/202/messages/1234567890123456789",
		"/accounts/acct/folders/404/messages/1234567890123456790",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("paths = %v, want %v", paths, wantPaths)
	}
	for _, method := range methods {
		if method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", method)
		}
	}
	for i, query := range queries {
		if query.Get("expunge") != "true" {
			t.Errorf("request %d sent expunge=%q; without it Zoho moves the message to Trash "+
				"and this method has promised to destroy it", i, query.Get("expunge"))
		}
	}
}

// Discarding a draft and destroying a message are the same Zoho endpoint, and expunge is the
// only thing that separates them. Asserting the pair together is what stops the two being
// tidied into one call: a discard that gained the flag would destroy somebody's draft, and a
// delete that lost it would report mail destroyed that is sitting in Trash.
func TestDiscardingAndDeletingDifferOnlyInExpunge(t *testing.T) {
	var seen []string
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("expunge"))
		writeEnvelope(t, w, map[string]any{"cId": 1234567890123456789})
	})

	id := mmail.ScopedID{Account: "acct_1", Native: "300/1234567890123456789"}
	if err := p.DeleteDraft(context.Background(), id); err != nil {
		t.Fatalf("discarding a draft: %v", err)
	}
	if err := p.Delete(context.Background(), []mmail.ScopedID{id}); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	if !slices.Equal(seen, []string{"", "true"}) {
		t.Errorf("expunge on [discard, delete] = %v, want [\"\" true]: a discard is recoverable "+
			"and a delete is not", seen)
	}
}

// Nothing this method destroys can be put back, so a batch it cannot fully read must destroy
// none of it. Parsing on the way through the loop would delete every message up to the bad id
// and then report a parse error, which reads as though nothing happened.
func TestDeletingDestroysNothingWhenAnIDInTheBatchIsMalformed(t *testing.T) {
	var requests int
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeEnvelope(t, w, map[string]any{"cId": 1234567890123456789})
	})

	err := p.Delete(context.Background(), []mmail.ScopedID{
		{Account: "acct_1", Native: "202/1234567890123456789"},
		{Account: "acct_1", Native: "1234567890123456790"},
	})
	if err == nil {
		t.Fatal("a native id without a folder must be refused")
	}
	if requests != 0 {
		t.Errorf("%d message(s) were destroyed before the batch was found to be unreadable", requests)
	}
}

// The same HTTP 200 with a failing envelope, on the call where believing it costs the most: a
// caller told the mail is gone stops looking for it.
func TestADeleteThatFailedInTheEnvelopeIsNotASuccess(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 500, "description": "Internal Error"},
		})
	})

	if err := p.Delete(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "202/1234567890123456789"}}); err == nil {
		t.Fatal("a failing envelope under HTTP 200 must not be read as mail destroyed")
	}
}
