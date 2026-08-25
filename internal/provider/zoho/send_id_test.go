package zoho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Zoho answers a send with a messageId and no folderId, and every id in this provider is
// <folder>/<message>. Handing back what the response contained produced
// "/1234567890123456791", which splitNative rejects — so a caller that sent a message and
// then tried to read it, label it or reply to it got an error naming its own id, for a
// message that had been sent perfectly well.
func TestSendReturnsAUsableID(t *testing.T) {
	var folderCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/folders"):
			folderCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"code": 200},
				"data": []map[string]any{
					// A user folder that happens to be called Sent comes first on purpose:
					// picking it would hand back an id addressing the wrong folder, which
					// resolves and is wrong — the worst combination.
					{"folderId": "111", "folderName": "Sent", "isSystemFolder": false},
					{"folderId": "222", "folderName": "Sent", "isSystemFolder": true},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"code": 200},
				"data":   map[string]any{"messageId": "1234567890123456791"},
			})
		}
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}

	id, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Subject: "hello",
		Body:    mmail.Body{Text: "hello"},
	})
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if id.Native != "222/1234567890123456791" {
		t.Fatalf("id = %q, want the system Sent folder and the message id", id.Native)
	}
	if _, _, err := splitNative(id.Native); err != nil {
		t.Errorf("the id a send returns must be one this provider accepts: %v", err)
	}
	if folderCalls != 1 {
		t.Errorf("the folder list was read %d times, want once", folderCalls)
	}
}

// A folder Zoho does report is used as-is, without the extra request.
func TestSendUsesTheFolderZohoReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/folders") {
			t.Error("the folder list should not be read when the send response names one")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 200},
			"data":   map[string]any{"messageId": "999", "folderId": "555"},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work"},
	}
	id, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1", To: []mmail.Address{{Email: "a@b.com"}}, Body: mmail.Body{Text: "x"},
	})
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if id.Native != "555/999" {
		t.Fatalf("id = %q, want 555/999", id.Native)
	}
}

// If the folder cannot be resolved there is no honest id to give back. An id that cannot
// address the message is worse than none, because it is only discovered at the point
// somebody uses it.
func TestSendRefusesToInventAFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/folders") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"code": 200},
				"data":   []map[string]any{{"folderId": "1", "folderName": "Inbox", "isSystemFolder": true}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 200},
			"data":   map[string]any{"messageId": "999"},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work"},
	}
	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1", To: []mmail.Address{{Email: "a@b.com"}}, Body: mmail.Body{Text: "x"},
	}); err == nil {
		t.Fatal("want an error naming that the message was sent but its id could not be resolved")
	} else if !strings.Contains(err.Error(), "was sent") {
		t.Errorf("the error must say the message did go out: %v", err)
	}
}
