package imap

import (
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

func mime(lines ...string) []byte { return []byte(strings.Join(lines, "\r\n")) }

// Which part becomes the HTML body, across the shapes real mail takes. The HTML part is where
// the links are, so losing it — or taking an attachment for it — loses them.
func TestTheHTMLBodyIsFoundInEveryShape(t *testing.T) {
	cases := []struct {
		name         string
		raw          []byte
		text, html   string
		attachments  int
		inlineImages int
	}{
		{
			name: "multipart/alternative",
			raw: mime(
				"Content-Type: multipart/alternative; boundary=ALT", "",
				"--ALT", "Content-Type: text/plain; charset=utf-8", "", "Fill in [Form]",
				"--ALT", "Content-Type: text/html; charset=utf-8", "", `<a href="https://example.com/f">Form</a>`,
				"--ALT--", ""),
			text: "Fill in [Form]", html: `<a href="https://example.com/f">Form</a>`,
		},
		{
			name: "html only",
			raw: mime(
				"Content-Type: text/html; charset=utf-8", "",
				`<p>Only <a href="https://example.com/x">HTML</a></p>`),
			html: `<p>Only <a href="https://example.com/x">HTML</a></p>`,
		},
		{
			// mixed > related > alternative, with an inline logo inside related and a real
			// attachment beside it: the shape most newsletters and receipts arrive in.
			name: "nested multipart with an inline image",
			raw: mime(
				"Content-Type: multipart/mixed; boundary=MIX", "",
				"--MIX", "Content-Type: multipart/related; boundary=REL", "",
				"--REL", "Content-Type: multipart/alternative; boundary=ALT", "",
				"--ALT", "Content-Type: text/plain; charset=utf-8", "", "plain",
				"--ALT", "Content-Type: text/html; charset=utf-8", "", `<img src="cid:logo"><a href="https://example.com/n">Nested</a>`,
				"--ALT--",
				"--REL", "Content-Type: image/png; name=logo.png", "Content-ID: <logo>",
				"Content-Disposition: inline; filename=logo.png", "", "PNGDATA",
				"--REL--",
				"--MIX", "Content-Type: text/html; name=invoice.html",
				"Content-Disposition: attachment; filename=invoice.html", "", "<p>not the body</p>",
				"--MIX--", ""),
			text: "plain", html: `<img src="cid:logo"><a href="https://example.com/n">Nested</a>`,
			attachments: 2, inlineImages: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out mmail.Message
			if err := parseBody(tc.raw, &out); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(out.Body.Text); got != tc.text {
				t.Errorf("text = %q, want %q", got, tc.text)
			}
			if got := strings.TrimSpace(out.Body.HTML); got != tc.html {
				t.Errorf("html = %q, want %q", got, tc.html)
			}
			if len(out.Attachments) != tc.attachments {
				t.Errorf("attachments = %+v, want %d", out.Attachments, tc.attachments)
			}
			inline := 0
			for _, a := range out.Attachments {
				if a.Inline {
					inline++
				}
			}
			if inline != tc.inlineImages {
				t.Errorf("inline attachments = %d, want %d", inline, tc.inlineImages)
			}
		})
	}
}
