package zoho

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// uploadedPart is one file as it actually arrived at Zoho, read back out of the multipart
// body rather than out of anything this package chose to remember.
type uploadedPart struct {
	field    string
	filename string
	mimeType string
	content  string
}

// readUploadParts decodes the multipart body of an upload request.
//
// The assertions in this file are on what reaches Zoho, so the body has to be parsed the way
// Zoho would parse it: a part whose Content-Disposition is malformed, or whose field name is
// not the one the API takes, is a file that never becomes an attachment however good the
// bytes inside it are.
func readUploadParts(t *testing.T, r *http.Request) []uploadedPart {
	t.Helper()

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("the upload's Content-Type did not parse: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, want multipart/form-data: Zoho's upload route takes a form, "+
			"not JSON", mediaType)
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	var parts []uploadedPart
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the multipart body: %v", err)
		}
		content, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("reading a part: %v", err)
		}
		parts = append(parts, uploadedPart{
			field:    part.FormName(),
			filename: part.FileName(),
			mimeType: part.Header.Get("Content-Type"),
			content:  string(content),
		})
	}
	return parts
}

// A send with attachments is two calls in a fixed order: the files are stored, and only then
// is a message composed referencing them.
//
// Everything asserted here is on the wire rather than on the id that comes back. The field
// names are Zoho's published ones — "attach" on the upload
// (https://www.zoho.com/mail/help/api/post-upload-attachments.html) and the storeName,
// attachmentPath and attachmentName triple on the compose
// (https://www.zoho.com/mail/help/api/post-send-email-attachment.html) — and a request that
// spelled any of them differently would not fail loudly, it would send a message with nothing
// attached.
func TestSendUploadsAttachmentsAndReferencesThem(t *testing.T) {
	var order []string
	var parts []uploadedPart
	var composed map[string]any

	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/attachments"):
			order = append(order, "upload")
			if r.Method != http.MethodPost {
				t.Errorf("upload method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/accounts/acct/messages/attachments" {
				t.Errorf("upload path = %q, want /accounts/acct/messages/attachments", r.URL.Path)
			}
			if got := r.URL.Query().Get("uploadType"); got != "multipart" {
				t.Errorf("uploadType = %q, want multipart — Zoho keys the response shape off it", got)
			}
			if got := r.URL.Query().Get("isInline"); got != "false" {
				t.Errorf("isInline = %q, want false: mailroom never references an embedded part "+
					"from a body, so an inline file would be one the message never mentions", got)
			}
			parts = readUploadParts(t, r)
			writeEnvelope(t, w, []map[string]any{
				{
					"attachmentSize": "6",
					"storeName":      "52882865",
					"attachmentName": "notes.txt",
					"attachmentPath": "/Mail/7db8a3aa5d5c12681bb51-notes.txt",
				},
				{
					"attachmentSize": "4",
					"storeName":      "NN2:-167775813820412438",
					"attachmentName": "chart.png",
					"attachmentPath": "/Mail/5ea951795d5c126825b7a-chart.png",
				},
			})

		case strings.HasSuffix(r.URL.Path, "/messages"):
			order = append(order, "compose")
			composed = decodeBody(t, r)
			writeEnvelope(t, w, map[string]any{"messageId": "999", "folderId": "222"})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	id, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Subject: "see attached",
		Body:    mmail.Body{Text: "two files"},
		Attachments: []mmail.Attachment{
			{
				AttachmentRef: mmail.AttachmentRef{Filename: "notes.txt", MimeType: "text/plain"},
				Content:       []byte("hello!"),
			},
			{
				AttachmentRef: mmail.AttachmentRef{Filename: "chart.png", MimeType: "image/png"},
				Content:       []byte("\x89PNG"),
			},
		},
	})
	if err != nil {
		t.Fatalf("sending with attachments: %v", err)
	}
	if id.Native != "222/999" {
		t.Errorf("id = %q, want 222/999", id.Native)
	}

	if len(order) != 2 || order[0] != "upload" || order[1] != "compose" {
		t.Fatalf("call order = %v, want the upload before the compose: a message composed first "+
			"could go out without its files", order)
	}

	if len(parts) != 2 {
		t.Fatalf("Zoho received %d files, want 2: %+v", len(parts), parts)
	}
	want := []uploadedPart{
		{field: "attach", filename: "notes.txt", mimeType: "text/plain", content: "hello!"},
		{field: "attach", filename: "chart.png", mimeType: "image/png", content: "\x89PNG"},
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("part %d = %+v, want %+v", i+1, parts[i], w)
		}
	}

	refs, ok := composed["attachments"].([]any)
	if !ok {
		t.Fatalf("the compose body carries no attachments array: %+v", composed)
	}
	if len(refs) != 2 {
		t.Fatalf("the compose body references %d attachments, want 2", len(refs))
	}
	first, _ := refs[0].(map[string]any)
	if first["storeName"] != "52882865" ||
		first["attachmentPath"] != "/Mail/7db8a3aa5d5c12681bb51-notes.txt" ||
		first["attachmentName"] != "notes.txt" {
		t.Errorf("the first reference is not the handle Zoho returned: %+v", first)
	}
	// The second store name is Zoho's other published spelling. It is not a number, and a
	// reference that dropped it or re-spelled it names a file the send cannot resolve.
	second, _ := refs[1].(map[string]any)
	if second["storeName"] != "NN2:-167775813820412438" {
		t.Errorf("the second store name did not survive: %+v", second)
	}
}

// The failure direction, which is the whole reason this path exists. An upload that fails must
// leave nothing sent — not a message with a file missing, which nobody discovers until the
// recipient asks where it is.
func TestAFailedUploadSendsNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"code":500,"description":"Internal Error"}}`))
			return
		}
		t.Errorf("nothing should be composed after a failed upload: %s %s", r.Method, r.URL.Path)
	})

	id, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "a.txt"}, Content: []byte("x")}},
	})
	if err == nil {
		t.Fatalf("want an error, got the id %q", id.Native)
	}
	if !id.Zero() {
		t.Errorf("an id must not come back alongside the error: %q", id.Native)
	}
}

// Zoho can answer HTTP 200 with a failure inside the envelope, and this endpoint is written by
// hand rather than going through do — so it has to make that check itself. Reading this as a
// success would compose a message referencing files that were never stored.
//
// The data below is deliberately well formed. A failing envelope wrapped around an empty body
// is caught by the checks further down whether the envelope is read or not, so it would prove
// nothing about the one under test.
func TestAFailingUploadEnvelopeUnderA200SendsNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":{"code":500,"description":"Storage unavailable"},` +
				`"data":[{"storeName":"52882865","attachmentName":"a.txt","attachmentPath":"/Mail/aaa-a.txt"}]}`))
			return
		}
		t.Errorf("nothing should be composed: %s %s", r.Method, r.URL.Path)
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "a.txt"}, Content: []byte("x")}},
	}); err == nil {
		t.Fatal("want an error: a 200 carrying a failing envelope is not a stored file")
	}
}

// An expired credential has to survive this route as an expired credential. Zoho answers a
// dead token with a 401 carrying no status object, so the envelope check below is no help and
// the HTTP status is the only thing that says what happened — and getting it wrong here costs
// more than one send, because a mailbox reported as a generic failure is never marked for
// re-linking and every later call fails obscurely too.
func TestAnExpiredTokenOnUploadIsReportedAsSuch(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"data":{"errorCode":"INVALID_OAUTHTOKEN"}}`))
			return
		}
		t.Errorf("nothing should be composed: %s %s", r.Method, r.URL.Path)
	})

	_, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "a.txt"}, Content: []byte("x")}},
	})
	if !errors.Is(err, mmail.ErrNeedsReauth) {
		t.Fatalf("err = %v, want the re-link error so the mailbox is marked rather than retried", err)
	}
}

// A half-done upload is the subtle version of the same failure: Zoho answers successfully and
// accounts for fewer files than went up. Composing anyway would send the message with exactly
// the files Zoho did not mention missing from it.
func TestAPartialUploadSendsNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			writeEnvelope(t, w, []map[string]any{{
				"storeName":      "52882865",
				"attachmentName": "one.txt",
				"attachmentPath": "/Mail/aaa-one.txt",
			}})
			return
		}
		t.Errorf("a message must not be composed from a partial upload: %s %s", r.Method, r.URL.Path)
	})

	_, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Body:    mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{
			{AttachmentRef: mmail.AttachmentRef{Filename: "one.txt"}, Content: []byte("1")},
			{AttachmentRef: mmail.AttachmentRef{Filename: "two.txt"}, Content: []byte("2")},
		},
	})
	if err == nil {
		t.Fatal("want an error naming that only some of the attachments were stored")
	}
	if !strings.Contains(err.Error(), "nothing has been sent") {
		t.Errorf("the error must say the message did not go: %v", err)
	}
}

// An entry that arrives without all three of the fields a compose call references it by
// addresses nothing. Sending it would hand Zoho a reference it cannot resolve, and the
// recipient would be the one to find out.
func TestAnUnaddressableUploadResultSendsNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			writeEnvelope(t, w, []map[string]any{{
				"attachmentName": "one.txt",
				"attachmentPath": "/Mail/aaa-one.txt",
			}})
			return
		}
		t.Errorf("a message must not be composed from an unaddressable upload: %s %s",
			r.Method, r.URL.Path)
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "one.txt"}, Content: []byte("1")}},
	}); err == nil {
		t.Fatal("want an error: an upload with no store name cannot be attached to anything")
	}
}

// Zoho documents an array for the multipart upload and a bare object for the raw one, and this
// package has already been caught by an account answering a different endpoint in the shape
// the other method's page documents. Both are accepted, so a mailbox that answers the object
// form still gets its attachment rather than silence.
func TestASingleObjectUploadResponseIsAccepted(t *testing.T) {
	var composed map[string]any
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			writeEnvelope(t, w, map[string]any{
				"storeName":      "53862395",
				"attachmentName": "one.txt",
				"attachmentPath": "/Mail/4f7e6dfd5e6f952bccf41-one.txt",
			})
			return
		}
		composed = decodeBody(t, r)
		writeEnvelope(t, w, map[string]any{"messageId": "999", "folderId": "222"})
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "one.txt"}, Content: []byte("1")}},
	}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	refs, ok := composed["attachments"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("the object-shaped response produced no attachment reference: %+v", composed)
	}
	if first, _ := refs[0].(map[string]any); first["storeName"] != "53862395" {
		t.Errorf("reference = %+v, want the handle from the object-shaped response", refs[0])
	}
}

// Two limits apply and mailroom's is the lower one, so mailroom's is what refuses. Naming the
// wrong one sends somebody to their Zoho plan for a ceiling that is not in the way — and the
// refusal has to come before the upload, or a message that cannot be sent still spends the
// bandwidth of every file on it.
func TestOversizedAttachmentsAreRefusedBeforeUploading(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should reach Zoho: %s %s", r.Method, r.URL.Path)
	})

	_, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Body:    mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{
			AttachmentRef: mmail.AttachmentRef{Filename: "huge.bin"},
			Content:       make([]byte, mmail.MaxAttachmentBytes+1),
		}},
	})

	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want an unsupported_by_provider error, got %v", err)
	}
	if !strings.Contains(unsupported.Reason, "mailroom") {
		t.Errorf("the refusal must name which limit did the refusing: %q", unsupported.Reason)
	}
}

// The size check sums the message rather than looking at one file at a time: mailroom's limit
// is on what a whole message carries, and several files that each fit can still add up to one
// that will not go.
func TestAttachmentsAreSizedTogether(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should reach Zoho: %s %s", r.Method, r.URL.Path)
	})

	half := mmail.MaxAttachmentBytes/2 + 1
	_, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Body:    mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{
			{AttachmentRef: mmail.AttachmentRef{Filename: "a.bin"}, Content: make([]byte, half)},
			{AttachmentRef: mmail.AttachmentRef{Filename: "b.bin"}, Content: make([]byte, half)},
		},
	})

	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want an unsupported_by_provider error for two files that fit individually, got %v", err)
	}
}

// A draft carries its files the same way a send does. A draft that lost them is worse than a
// send that did: the loss is discovered by whoever opens the draft to send it, and by then it
// looks like something they did.
func TestADraftUploadsItsAttachments(t *testing.T) {
	var uploads int
	var saved map[string]any

	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/attachments"):
			uploads++
			writeEnvelope(t, w, []map[string]any{{
				"storeName":      "52882865",
				"attachmentName": "draft.txt",
				"attachmentPath": "/Mail/ccc-draft.txt",
			}})
		case strings.HasSuffix(r.URL.Path, "/folders"):
			writeEnvelope(t, w, draftsFolderListing()["data"])
		default:
			saved = decodeBody(t, r)
			writeEnvelope(t, w, map[string]any{"messageId": "1234567890123456789"})
		}
	})

	if _, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "draft.txt"}, Content: []byte("d")}},
	}); err != nil {
		t.Fatalf("saving a draft with attachments: %v", err)
	}

	if uploads != 1 {
		t.Errorf("the upload endpoint was called %d times, want once", uploads)
	}
	if saved["mode"] != "draft" {
		t.Errorf("mode = %v, want draft — without it Zoho sends the message", saved["mode"])
	}
	refs, ok := saved["attachments"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("the saved draft references no attachment: %+v", saved)
	}
}

// A draft whose files could not be stored must not be saved either, for the same reason a send
// is not: a draft that looks complete and is not gets sent by somebody who trusted it.
func TestAFailedUploadSavesNoDraft(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"code":500,"description":"Internal Error"}}`))
			return
		}
		t.Errorf("no draft should be saved after a failed upload: %s %s", r.Method, r.URL.Path)
	})

	if _, err := p.CreateDraft(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{AttachmentRef: mmail.AttachmentRef{Filename: "a.txt"}, Content: []byte("x")}},
	}); err == nil {
		t.Fatal("want an error rather than a draft saved without its attachment")
	}
}

// A quote in a filename would close the Content-Disposition parameter early, and the file
// would be stored under a truncated name — which is the name the compose call has to reference
// it by, so the attachment would be lost to a character that is legal in a filename.
func TestAFilenameWithAQuoteSurvivesTheFormEncoding(t *testing.T) {
	var parts []uploadedPart
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			parts = readUploadParts(t, r)
			writeEnvelope(t, w, []map[string]any{{
				"storeName":      "52882865",
				"attachmentName": `say "hi".txt`,
				"attachmentPath": "/Mail/ddd-hi.txt",
			}})
			return
		}
		writeEnvelope(t, w, map[string]any{"messageId": "999", "folderId": "222"})
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Body:    mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{
			AttachmentRef: mmail.AttachmentRef{Filename: `say "hi".txt`},
			Content:       []byte("q"),
		}},
	}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	if len(parts) != 1 {
		t.Fatalf("Zoho received %d parts, want 1: %+v", len(parts), parts)
	}
	if parts[0].filename != `say "hi".txt` {
		t.Errorf("filename = %q, want it intact through the form encoding", parts[0].filename)
	}
}

// An attachment with no type of its own still has to be uploadable, and it must not arrive
// with an empty Content-Type: a part with no type is not a file Zoho can store as one.
func TestAnUntypedAttachmentGetsADefaultType(t *testing.T) {
	var parts []uploadedPart
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/attachments") {
			parts = readUploadParts(t, r)
			writeEnvelope(t, w, []map[string]any{{
				"storeName":      "52882865",
				"attachmentName": "attachment",
				"attachmentPath": "/Mail/eee-attachment",
			}})
			return
		}
		writeEnvelope(t, w, map[string]any{"messageId": "999", "folderId": "222"})
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account:     "acct_1",
		To:          []mmail.Address{{Email: "someone@example.com"}},
		Body:        mmail.Body{Text: "see attached"},
		Attachments: []mmail.Attachment{{Content: []byte("u")}},
	}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	if len(parts) != 1 {
		t.Fatalf("Zoho received %d parts, want 1: %+v", len(parts), parts)
	}
	if parts[0].mimeType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", parts[0].mimeType)
	}
	if parts[0].filename == "" {
		t.Error("a part with no filename has nothing for the compose call to reference it by")
	}
}

// A message with no attachments must not touch the upload endpoint at all. The check that
// keeps this true is one line, and without it every plain send pays for a request that stores
// nothing.
func TestASendWithoutAttachmentsUploadsNothing(t *testing.T) {
	p := zohoProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/attachments") {
			t.Errorf("a message with no attachments must not call the upload endpoint: %s", r.URL.Path)
		}
		writeEnvelope(t, w, map[string]any{"messageId": "999", "folderId": "222"})
	})

	if _, err := p.Send(context.Background(), mmail.Outgoing{
		Account: "acct_1",
		To:      []mmail.Address{{Email: "someone@example.com"}},
		Body:    mmail.Body{Text: "nothing attached"},
	}); err != nil {
		t.Fatalf("sending: %v", err)
	}
}
