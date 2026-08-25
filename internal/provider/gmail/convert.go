package gmail

import (
	"encoding/base64"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// convert maps a Gmail message onto the canonical model. withBody is false for search
// results, where only metadata was requested.
func (p *Provider) convert(m *gmail.Message, withBody bool) mmail.Message {
	out := mmail.Message{
		ID:       mmail.ScopedID{Account: p.account.ID, Native: m.Id},
		Account:  p.account.Alias,
		ThreadID: mmail.ScopedID{Account: p.account.ID, Native: m.ThreadId},
		Snippet:  strings.TrimSpace(m.Snippet),
		Date:     time.UnixMilli(m.InternalDate).UTC(),
	}

	for _, id := range m.LabelIds {
		out.Labels = append(out.Labels, mmail.LabelID(id))
		switch id {
		case "UNREAD":
			// Gmail marks what is unread; the model carries the positive form.
			out.Flags.Read = false
		case "STARRED":
			out.Flags.Starred = true
		case "DRAFT":
			out.Flags.Draft = true
		}
	}
	out.Flags.Read = !contains(m.LabelIds, "UNREAD")

	if m.Payload != nil {
		readHeaders(m.Payload.Headers, &out)
		if withBody {
			collectParts(m.Payload, &out)
		} else {
			// Even without bodies, attachment names and sizes are part of the metadata a
			// caller may see under `read` — only the contents need the extra capability.
			collectAttachmentRefs(m.Payload, &out)
		}
	}
	return out
}

func readHeaders(headers []*gmail.MessagePartHeader, out *mmail.Message) {
	for _, h := range headers {
		switch strings.ToLower(h.Name) {
		case "from":
			if addrs := parseAddresses(h.Value); len(addrs) > 0 {
				out.From = addrs[0]
			}
		case "to":
			out.To = append(out.To, parseAddresses(h.Value)...)
		case "cc":
			out.Cc = append(out.Cc, parseAddresses(h.Value)...)
		case "bcc":
			out.Bcc = append(out.Bcc, parseAddresses(h.Value)...)
		case "subject":
			out.Subject = h.Value
		case "date":
			// InternalDate is authoritative for ordering; the header only fills in when
			// Gmail did not supply one.
			if out.Date.IsZero() {
				if t, err := mail.ParseDate(h.Value); err == nil {
					out.Date = t.UTC()
				}
			}
		}
	}
}

func parseAddresses(v string) []mmail.Address {
	parsed, err := mail.ParseAddressList(v)
	if err != nil {
		// Malformed address lists are common in real mail. Keep the raw value rather than
		// dropping the field, so a human can still see who it claims to be from.
		if v = strings.TrimSpace(v); v != "" {
			return []mmail.Address{{Email: v}}
		}
		return nil
	}
	out := make([]mmail.Address, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, mmail.Address{Name: a.Name, Email: a.Address})
	}
	return out
}

// collectParts walks the MIME tree, taking the first text and HTML bodies it finds and
// recording every attachment.
func collectParts(part *gmail.MessagePart, out *mmail.Message) {
	if part == nil {
		return
	}

	filename := strings.TrimSpace(part.Filename)
	if filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
		out.Attachments = append(out.Attachments, attachmentRef(part))
	} else if part.Body != nil && part.Body.Data != "" {
		if data, err := decodeBase64URL(part.Body.Data); err == nil {
			switch {
			case strings.HasPrefix(part.MimeType, "text/plain") && out.Body.Text == "":
				out.Body.Text = string(data)
			case strings.HasPrefix(part.MimeType, "text/html") && out.Body.HTML == "":
				out.Body.HTML = string(data)
			}
		}
	}

	for _, child := range part.Parts {
		collectParts(child, out)
	}
}

func collectAttachmentRefs(part *gmail.MessagePart, out *mmail.Message) {
	if part == nil {
		return
	}
	if part.Filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
		out.Attachments = append(out.Attachments, attachmentRef(part))
	}
	for _, child := range part.Parts {
		collectAttachmentRefs(child, out)
	}
}

func attachmentRef(part *gmail.MessagePart) mmail.AttachmentRef {
	ref := mmail.AttachmentRef{
		ID:       part.Body.AttachmentId,
		Filename: part.Filename,
		MimeType: part.MimeType,
		Size:     part.Body.Size,
	}
	for _, h := range part.Headers {
		if strings.EqualFold(h.Name, "Content-Disposition") && strings.Contains(strings.ToLower(h.Value), "inline") {
			ref.Inline = true
		}
		if strings.EqualFold(h.Name, "Content-Length") && ref.Size == 0 {
			if n, err := strconv.ParseInt(h.Value, 10, 64); err == nil {
				ref.Size = n
			}
		}
	}
	return ref
}

// decodeBase64URL accepts both padded and unpadded forms; Gmail is inconsistent about which
// it returns.
func decodeBase64URL(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
