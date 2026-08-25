package imap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The bytes a client is really after. Not text: a payload that survives base64 but not a
// charset conversion is what proves the content came back raw.
func binaryPayload() []byte {
	out := make([]byte, 3001)
	for i := range out {
		out[i] = byte((i * 37) % 256)
	}
	return out
}

// spreadsheet is quoted-printable in the message, with a euro sign and a literal equals
// sign: the two characters that come back wrong if the encoding is not undone.
const spreadsheet = "item,total\r\nwidget,€12.50\r\nformula,=SUM(A1:A2)\r\n"

// withAttachments is one message shaped the way real mail is: the body is a nested
// multipart/alternative, so the attachments are not the parts a naive walk would number them.
func withAttachments(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("From: Sender <sender@example.com>\r\n")
	b.WriteString("To: operator@example.com\r\n")
	b.WriteString("Subject: Quarterly figures\r\n")
	b.WriteString("Date: Mon, 03 Aug 2026 12:00:00 +0000\r\n")
	b.WriteString("Message-ID: <figures@example.com>\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"MIXED\"\r\n\r\n")

	b.WriteString("--MIXED\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n")
	b.WriteString("--ALT\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("The figures are attached.\r\n")
	b.WriteString("--ALT\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString("<p>The figures are attached.</p>\r\n")
	b.WriteString("--ALT--\r\n")

	b.WriteString("--MIXED\r\n")
	b.WriteString("Content-Type: application/pdf; name=\"figures.pdf\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"figures.pdf\"\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString(binaryPayload())
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")

	b.WriteString("--MIXED\r\n")
	b.WriteString("Content-Type: text/csv; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"figures.csv\"\r\n\r\n")
	// The final "=" is a soft line break, so the line the message ends on adds no newline of
	// its own to the file.
	b.WriteString("item,total=0D=0Awidget,=E2=82=AC12.50=0D=0Aformula,=3DSUM(A1:A2)=0D=0A=\r\n")
	b.WriteString("--MIXED--\r\n")

	return b.String()
}

func providerWith(t *testing.T, raws ...string) *Provider {
	t.Helper()
	addr, _, user, pass := startServerWith(t, raws)

	p, err := New(context.Background(), mmail.Account{
		ID: "acct_imap", Alias: "imap", Address: "operator@example.com",
		Provider: mmail.ProviderIMAP, Status: mmail.StatusLinked,
	}, Config{Host: addr, Username: user, Password: pass})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// GetAttachment used to return the manifest entry with no Content, so mail_get_attachment
// answered with a zero-byte file and a success status — which a client cannot tell from an
// attachment that is genuinely empty. Nothing short of comparing the bytes to the original
// catches that: a length check against a manifest size the same walk produced agrees with
// itself, and both numbers look right.
func TestGetAttachmentReturnsTheBytesThatWereSent(t *testing.T) {
	p := providerWith(t, withAttachments(t))
	ctx := context.Background()
	id := mmail.ScopedID{Account: "acct_imap", Native: "INBOX/1"}

	msg, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("reading the message: %v", err)
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("want 2 attachments in the manifest, got %d: %+v", len(msg.Attachments), msg.Attachments)
	}
	if msg.Body.Text != "The figures are attached." {
		t.Errorf("the body text is wrong: %q", msg.Body.Text)
	}
	if msg.Body.HTML == "" {
		t.Error("the html part is missing from the body")
	}

	want := map[string][]byte{
		"figures.pdf": binaryPayload(),
		"figures.csv": []byte(spreadsheet),
	}
	sections := map[string]string{"figures.pdf": "2", "figures.csv": "3"}

	for _, ref := range msg.Attachments {
		expected, ok := want[ref.Filename]
		if !ok {
			t.Fatalf("unexpected attachment %q", ref.Filename)
		}
		if ref.ID != sections[ref.Filename] {
			t.Errorf("%s: want section %q as the id, got %q", ref.Filename, sections[ref.Filename], ref.ID)
		}
		if ref.Size != int64(len(expected)) {
			t.Errorf("%s: manifest size %d, file is %d bytes", ref.Filename, ref.Size, len(expected))
		}

		att, err := p.GetAttachment(ctx, id, ref.ID)
		if err != nil {
			t.Fatalf("%s: fetching: %v", ref.Filename, err)
		}
		if digest(att.Content) != digest(expected) {
			t.Errorf("%s: content differs from the original: %d bytes, sha256 %s, want %d bytes, sha256 %s",
				ref.Filename, len(att.Content), digest(att.Content), len(expected), digest(expected))
		}
		if att.Filename != ref.Filename || att.MimeType != ref.MimeType {
			t.Errorf("%s: the fetched part describes itself differently from the manifest: %+v", ref.Filename, att.AttachmentRef)
		}
	}
}

// A message that is not multipart is section 1 in its entirety, which is the numbering rule
// most easily got wrong by one.
func TestGetAttachmentOnAMessageThatIsOnlyAnAttachment(t *testing.T) {
	raw := "From: Sender <sender@example.com>\r\n" +
		"To: operator@example.com\r\n" +
		"Subject: Just the file\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/csv; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=\"figures.csv\"\r\n\r\n" +
		"item,total=0D=0Awidget,=E2=82=AC12.50=0D=0Aformula,=3DSUM(A1:A2)=0D=0A=\r\n"

	p := providerWith(t, raw)
	ctx := context.Background()
	id := mmail.ScopedID{Account: "acct_imap", Native: "INBOX/1"}

	msg, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("reading the message: %v", err)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ID != "1" {
		t.Fatalf("want one attachment at section 1, got %+v", msg.Attachments)
	}

	att, err := p.GetAttachment(ctx, id, msg.Attachments[0].ID)
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}
	if digest(att.Content) != digest([]byte(spreadsheet)) {
		t.Errorf("content differs from the original: %q", att.Content)
	}
}

// Anything that does not name a file has to be refused. Returning empty bytes with a success
// status is the failure being fixed, and handing back the message text under an attachment's
// name would be a new one.
func TestGetAttachmentRefusesWhatItCannotServe(t *testing.T) {
	p := providerWith(t, withAttachments(t))
	ctx := context.Background()
	id := mmail.ScopedID{Account: "acct_imap", Native: "INBOX/1"}

	// "1.1" is the text part of the body, and "figures.pdf" is the shape of id this provider
	// issued before the manifest carried section numbers.
	for _, attachmentID := range []string{"9", "1.1", "figures.pdf", ""} {
		att, err := p.GetAttachment(ctx, id, attachmentID)
		if !errors.Is(err, mmail.ErrNotFound) {
			t.Errorf("%q: want not found, got %v with %d bytes", attachmentID, err, len(att.Content))
		}
	}
}
