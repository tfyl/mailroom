// Package rfc5322 renders outgoing messages as wire-format mail.
//
// It lives outside any provider because message composition is not provider-specific: Gmail
// wants raw RFC 5322 and so does SMTP. Zoho, which takes structured fields instead, simply
// does not use this.
package rfc5322

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/tfyl/mailroom/internal/mail"
)

// Headers are encoded with mime.QEncoding rather than written raw: a subject or display name
// containing a newline would otherwise inject arbitrary headers, and the values here come
// from a model acting on mail written by strangers.
// Compose renders an outgoing message as RFC 5322 bytes.
func Compose(out mail.Outgoing, from string, replyTo *ReplyContext) ([]byte, error) {
	var buf bytes.Buffer

	writeHeader(&buf, "From", from)
	if len(out.To) > 0 {
		writeHeader(&buf, "To", joinAddresses(out.To))
	}
	if len(out.Cc) > 0 {
		writeHeader(&buf, "Cc", joinAddresses(out.Cc))
	}
	if len(out.Bcc) > 0 {
		writeHeader(&buf, "Bcc", joinAddresses(out.Bcc))
	}
	writeHeader(&buf, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeader(out.Subject)))

	// Threading headers. Without them a reply arrives as a new conversation, which is the
	// most common way an otherwise-correct reply looks broken to the recipient.
	if replyTo != nil {
		if replyTo.MessageID != "" {
			writeHeader(&buf, "In-Reply-To", replyTo.MessageID)
			references := strings.TrimSpace(replyTo.References + " " + replyTo.MessageID)
			writeHeader(&buf, "References", references)
		}
	}

	writeHeader(&buf, "MIME-Version", "1.0")

	if len(out.Attachments) == 0 {
		if out.Body.HTML != "" && out.Body.Text != "" {
			return writeAlternative(&buf, out)
		}
		contentType := "text/plain; charset=utf-8"
		body := out.Body.Text
		if body == "" && out.Body.HTML != "" {
			contentType, body = "text/html; charset=utf-8", out.Body.HTML
		}
		writeHeader(&buf, "Content-Type", contentType)
		buf.WriteString("\r\n")
		buf.WriteString(body)
		return buf.Bytes(), nil
	}

	return writeMixed(&buf, out)
}

// ReplyContext carries the threading headers a reply needs. Without them a reply arrives as
// a new conversation, which is the most common way an otherwise-correct reply looks broken.
type ReplyContext struct {
	MessageID  string
	References string
	ThreadID   string
}

func writeAlternative(buf *bytes.Buffer, out mail.Outgoing) ([]byte, error) {
	w := multipart.NewWriter(&bytes.Buffer{})
	boundary := w.Boundary()

	writeHeader(buf, "Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))
	buf.WriteString("\r\n")

	mw := multipart.NewWriter(buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, err
	}
	if err := writePart(mw, "text/plain; charset=utf-8", out.Body.Text); err != nil {
		return nil, err
	}
	if err := writePart(mw, "text/html; charset=utf-8", out.Body.HTML); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeMixed(buf *bytes.Buffer, out mail.Outgoing) ([]byte, error) {
	mw := multipart.NewWriter(&bytes.Buffer{})
	boundary := mw.Boundary()

	writeHeader(buf, "Content-Type", fmt.Sprintf("multipart/mixed; boundary=%q", boundary))
	buf.WriteString("\r\n")

	w := multipart.NewWriter(buf)
	if err := w.SetBoundary(boundary); err != nil {
		return nil, err
	}

	body := out.Body.Text
	contentType := "text/plain; charset=utf-8"
	if body == "" && out.Body.HTML != "" {
		body, contentType = out.Body.HTML, "text/html; charset=utf-8"
	}
	if err := writePart(w, contentType, body); err != nil {
		return nil, err
	}

	for _, att := range out.Attachments {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", attachmentType(att.MimeType))
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeHeader(att.Filename)))

		part, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if err := writeBase64Lines(part, att.Content); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// attachmentType renders an attachment's media type as something that cannot be anything but
// a media type.
//
// It is the one header value in this file that was written through untouched, and it is
// caller-controlled twice over: mail_upload_url takes it verbatim from an MCP client, and
// forwarding an attachment carries one written by a stranger. multipart.CreatePart writes a
// header value exactly as given, so a CR or LF in it appends headers of the caller's
// choosing — enough to override the `attachment` disposition the next line deliberately pins
// and have a payload render inline in the recipient's client.
//
// Parsed rather than merely stripped of CR and LF. Stripping leaves a header that is not
// syntactically dangerous but is still not a media type, and a value that is not a media
// type has no business being sent as one. The fallback is what every mail client already
// treats as opaque bytes.
func attachmentType(mediaType string) string {
	parsed, params, err := mime.ParseMediaType(sanitizeHeader(mediaType))
	if err != nil {
		return "application/octet-stream"
	}
	if formatted := mime.FormatMediaType(parsed, params); formatted != "" {
		return formatted
	}
	return "application/octet-stream"
}

func writePart(w *multipart.Writer, contentType, body string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(body))
	return err
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(sanitizeHeader(value))
	buf.WriteString("\r\n")
}

// sanitizeHeader strips CR and LF. Header injection is the one composition bug that turns a
// drafting tool into a way to add arbitrary recipients or headers, and the inputs here can
// originate in mail written by a stranger.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

func joinAddresses(addrs []mail.Address) string {
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		if a.Name != "" {
			parts[i] = mime.QEncoding.Encode("utf-8", sanitizeHeader(a.Name)) + " <" + sanitizeHeader(a.Email) + ">"
		} else {
			parts[i] = sanitizeHeader(a.Email)
		}
	}
	return strings.Join(parts, ", ")
}

// writeBase64Lines wraps at 76 characters, which RFC 2045 requires and some mail servers
// enforce by rejecting the message outright.
func writeBase64Lines(w io.Writer, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	const lineLen = 76
	for i := 0; i < len(encoded); i += lineLen {
		end := min(i+lineLen, len(encoded))
		if _, err := io.WriteString(w, encoded[i:end]+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}
