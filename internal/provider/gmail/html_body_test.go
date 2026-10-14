package gmail

import (
	"encoding/base64"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

func leaf(mimeType, data string) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: mimeType,
		Body:     &gmail.MessagePartBody{Data: base64.URLEncoding.EncodeToString([]byte(data))},
	}
}

func multipart(mimeType string, parts ...*gmail.MessagePart) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: mimeType, Body: &gmail.MessagePartBody{}, Parts: parts}
}

// Which part becomes the HTML body, across the shapes real mail takes. The HTML part is where
// the links are, so losing it — or taking an attached document for it — loses them.
func TestTheHTMLBodyIsFoundInEveryShape(t *testing.T) {
	const html = `<a href="https://example.com/f">Form</a>`

	logo := &gmail.MessagePart{
		MimeType: "image/png", Filename: "logo.png",
		Body: &gmail.MessagePartBody{AttachmentId: "att-logo", Size: 10},
		Headers: []*gmail.MessagePartHeader{
			{Name: "Content-Disposition", Value: "inline; filename=logo.png"},
		},
	}
	// Gmail inlines a small part's bytes even when it is a named file.
	attachedHTML := leaf("text/html", "<p>somebody's document</p>")
	attachedHTML.Filename = "invoice.html"

	cases := []struct {
		name        string
		payload     *gmail.MessagePart
		text, html  string
		attachments int
	}{
		{
			name:    "multipart/alternative",
			payload: multipart("multipart/alternative", leaf("text/plain", "Fill in [Form]"), leaf("text/html", html)),
			text:    "Fill in [Form]", html: html,
		},
		{
			name:    "html only",
			payload: leaf("text/html", html),
			html:    html,
		},
		{
			name: "nested multipart with an inline image and an attached html file",
			payload: multipart("multipart/mixed",
				attachedHTML,
				multipart("multipart/related",
					multipart("multipart/alternative", leaf("text/plain", "plain"), leaf("text/html", html)),
					logo)),
			text: "plain", html: html, attachments: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out mmail.Message
			collectParts(tc.payload, &out)
			if got := strings.TrimSpace(out.Body.Text); got != tc.text {
				t.Errorf("text = %q, want %q", got, tc.text)
			}
			if got := strings.TrimSpace(out.Body.HTML); got != tc.html {
				t.Errorf("html = %q, want %q", got, tc.html)
			}
			if len(out.Attachments) != tc.attachments {
				t.Errorf("attachments = %+v, want %d", out.Attachments, tc.attachments)
			}
		})
	}
}
