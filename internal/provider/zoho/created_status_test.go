package zoho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Zoho answers a label creation with an envelope status of 201 and the description
// "Created". Reading anything but 200 as a failure meant CreateLabel returned an error
// having created the label, so every call left one behind — observed against a live mailbox,
// where the probe label had to be cleaned up by hand.
func TestACreatedEnvelopeIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 201, "description": "Created"},
			"data":   map[string]any{"labelId": "12345", "displayName": "receipts"},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL,
		accountID: "acct", account: mmail.Account{ID: "acct_1", Alias: "work"},
	}

	label, err := p.CreateLabel(context.Background(), "receipts", false)
	if err != nil {
		t.Fatalf("a 201 envelope is success, got: %v", err)
	}
	if label.ID != "label:12345" || label.Name != "receipts" {
		t.Errorf("the created label was not decoded: %+v", label)
	}
}

// The envelope still has to be able to report a failure, including one arriving under HTTP
// 200 — which is the reason it is inspected separately from the response status.
func TestAFailingEnvelopeIsStillAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 500, "description": "Internal Error"},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL,
		accountID: "acct", account: mmail.Account{ID: "acct_1", Alias: "work"},
	}

	if _, err := p.CreateLabel(context.Background(), "receipts", false); err == nil {
		t.Fatal("a failing envelope under HTTP 200 must still be an error")
	}
}
