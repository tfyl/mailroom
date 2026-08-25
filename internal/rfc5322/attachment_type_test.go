package rfc5322

import (
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// An attachment's media type is caller-controlled twice over: mail_upload_url takes it
// verbatim from an MCP client, and forwarding an attachment carries one written by a
// stranger. multipart.CreatePart writes a header value exactly as given, so a CR or LF in it
// used to append headers of the caller's choosing — including overriding the `attachment`
// disposition the composer deliberately pins, so a payload renders inline in the recipient's
// client.
func TestAnAttachmentTypeCannotInjectHeaders(t *testing.T) {
	raw, err := Compose(mmail.Outgoing{
		To:      []mmail.Address{{Email: "you@example.com"}},
		Subject: "probe",
		Body:    mmail.Body{Text: "body"},
		Attachments: []mmail.Attachment{{
			AttachmentRef: mmail.AttachmentRef{
				Filename: "ok.txt",
				MimeType: "text/plain\r\nContent-Disposition: inline\r\nX-Injected: yes",
			},
			Content: []byte("hello"),
		}},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatalf("composing: %v", err)
	}

	got := string(raw)
	if strings.Contains(got, "X-Injected") {
		t.Error("a caller-supplied media type added a header of its own")
	}
	if strings.Contains(got, "Content-Disposition: inline") {
		t.Error("a caller-supplied media type overrode the attachment disposition")
	}
	if !strings.Contains(got, "Content-Disposition: attachment") {
		t.Error("the attachment disposition should still be pinned")
	}
}

// The fallback has to be reached without swallowing legitimate types, parameters included —
// a charset that stopped surviving would be a different bug in the same line.
func TestAttachmentTypeKeepsWhatIsRealAndRefusesWhatIsNot(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a plain type", "application/pdf", "application/pdf"},
		{"parameters survive", "text/plain; charset=utf-8", "text/plain; charset=utf-8"},
		{"empty falls back", "", "application/octet-stream"},
		{"not a media type at all", "definitely not a type", "application/octet-stream"},
		{"header injection falls back", "text/plain\r\nX-Evil: 1", "application/octet-stream"},
		{"a lone newline falls back", "text/plain\n", "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentType(tc.in); got != tc.want {
				t.Fatalf("attachmentType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
