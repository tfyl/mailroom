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

func zohoProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}
}

// The manifest is the only place an attachment id is ever produced, so without it the
// attachments capability is unreachable however well the download works. The body below is
// what the live mailbox returned, field names included — `attachmentSize`, and no content
// type at all.
func TestTheAttachmentManifestIsRead(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/attachmentinfo") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":{"code":200,"description":"success"},"data":{"attachments":[
			{"attachmentSize":697,"attachmentName":"report.zip","attachmentId":"140000000000000001"},
			{"attachmentSize":12,"attachmentName":"no-id.txt"}
		],"messageId":"1234567890123456790"}}`))
	})

	refs, err := p.attachmentRefs(context.Background(), "folder", "message")
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	// The second entry has no id, so it addresses nothing and must not be offered.
	if len(refs) != 1 {
		t.Fatalf("want 1 usable attachment, got %d: %+v", len(refs), refs)
	}
	if refs[0].ID != "140000000000000001" || refs[0].Filename != "report.zip" || refs[0].Size != 697 {
		t.Errorf("manifest decoded wrong: %+v", refs[0])
	}
}

// Zoho answers this endpoint with the file rather than with the envelope every other route
// uses, and refuses an Accept of application/json outright with 406 NOT_ACCEPTABLE.
func TestAnAttachmentIsFetchedAsBytes(t *testing.T) {
	const body = "PK\x03\x04 not really a zip"
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if accept := r.Header.Get("Accept"); strings.Contains(accept, "application/json") {
			w.WriteHeader(http.StatusNotAcceptable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"code": 406, "description": "Not Acceptable"},
			})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// Percent-encoded inside a plain filename= parameter, which is what the live mailbox
		// sends: taking it at face value yields a worse name than the manifest's.
		w.Header().Set("Content-Disposition", `attachment; filename="reporter.example%21mail.example.zip"`)
		_, _ = w.Write([]byte(body))
	})

	att, err := p.GetAttachment(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "folder/message"}, "140000000000000001")
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}
	if string(att.Content) != body {
		t.Errorf("content = %q, want %q", att.Content, body)
	}
	if att.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", att.Size, len(body))
	}
	if att.MimeType != "application/octet-stream" {
		t.Errorf("mime = %q, want application/octet-stream", att.MimeType)
	}
	if att.Filename != "reporter.example!mail.example.zip" {
		t.Errorf("filename = %q, want it percent-decoded", att.Filename)
	}
}

// A refusal must stay a refusal rather than becoming an attachment full of error text.
func TestAFailedAttachmentFetchIsAnError(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte(`{"status":{"code":406,"description":"Not Acceptable"}}`))
	})

	att, err := p.GetAttachment(context.Background(),
		mmail.ScopedID{Account: "acct_1", Native: "folder/message"}, "140000000000000001")
	if err == nil {
		t.Fatalf("want an error, got %d bytes of %q", len(att.Content), att.MimeType)
	}
}
