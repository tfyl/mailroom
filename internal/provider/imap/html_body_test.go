package imap

import (
	"context"
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

func htmlMessage(subject, id, link string) string {
	return strings.Join([]string{
		"From: Sender <sender@example.com>", "To: operator@example.com",
		"Subject: " + subject, "Date: Mon, 03 Aug 2026 12:00:00 +0000",
		"Message-ID: <" + id + "@example.com>", "MIME-Version: 1.0",
		"Content-Type: text/html; charset=utf-8", "",
		`<p><a href="` + link + `">Open</a></p>`, "",
	}, "\r\n")
}

// A derived thread is grouped from a search, which answers with envelopes alone. Every message
// but the one asked for used to come back with no body, so a thread's links were the first
// message's links and nobody else's — and reading them must not mark the rest as read.
func TestEveryMessageInAThreadHasItsBody(t *testing.T) {
	addr, _, user, pass := startServerWith(t, []string{
		htmlMessage("Change of ownership", "one", "https://example.com/1"),
		htmlMessage("Re: Change of ownership", "two", "https://example.com/2"),
	})
	p, err := New(context.Background(), mmail.Account{
		ID: "acct_imap", Alias: "imap", Address: "operator@example.com",
		Provider: mmail.ProviderIMAP, Status: mmail.StatusLinked,
	}, Config{Host: addr, Username: user, Password: pass, TLS: false})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	page, err := p.Search(context.Background(), mmail.Query{Subject: "Change of ownership"}, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("search: %v, %d items", err, len(page.Items))
	}
	var first mmail.Message
	for _, m := range page.Items {
		if !strings.HasPrefix(m.Subject, "Re:") {
			first = m
		}
	}

	thread, err := p.GetThread(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("thread has %d messages, want 2", len(thread.Messages))
	}
	for _, m := range thread.Messages {
		if !strings.Contains(m.Body.HTML, "https://example.com/") {
			t.Errorf("%q came back with no HTML body: %+v", m.Subject, m.Body)
		}
		if m.Flags.Read {
			t.Errorf("reading the thread marked %q as read", m.Subject)
		}
	}
}
