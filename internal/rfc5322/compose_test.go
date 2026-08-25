package rfc5322

import (
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

func to(addr string) []mail.Address { return []mail.Address{{Email: addr}} }

// headerNames lists the header field names actually present, which is what an injection test
// has to assert on.
//
// Checking that the payload text is absent would be the wrong test: once the CRLF is
// stripped, "Bcc: attacker@example.net" survives harmlessly *inside* a subject value. The
// property that matters is that no new header line was created.
func headerNames(raw []byte) []string {
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	var names []string
	for _, line := range strings.Split(head, "\r\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // continuation of the previous folded header
		}
		if name, _, ok := strings.Cut(line, ":"); ok {
			names = append(names, strings.ToLower(strings.TrimSpace(name)))
		}
	}
	return names
}

func hasHeader(raw []byte, name string) bool {
	for _, n := range headerNames(raw) {
		if n == strings.ToLower(name) {
			return true
		}
	}
	return false
}

// Header injection is the composition bug that turns a drafting tool into a way to add
// recipients nobody approved. These values can originate in mail written by a stranger and
// be relayed by a model, so a newline in a subject must never reach the wire.
func TestComposeStripsHeaderInjection(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To:      to("someone@example.com"),
		Subject: "Invoice\r\nBcc: attacker@example.net\r\nX-Evil: yes",
		Body:    mail.Body{Text: "hello"},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	if hasHeader(raw, "Bcc") {
		t.Errorf("the injected Bcc became a real header:\n%s", raw)
	}
	if hasHeader(raw, "X-Evil") {
		t.Errorf("the injected X-Evil became a real header:\n%s", raw)
	}

	// The payload survives as inert text inside the subject, which is fine. What must not
	// happen is it becoming its own header line.
	var subjects int
	for _, n := range headerNames(raw) {
		if n == "subject" {
			subjects++
		}
	}
	if subjects != 1 {
		t.Errorf("expected exactly one Subject header, got %d:\n%s", subjects, raw)
	}
}

func TestComposeStripsInjectionFromDisplayNames(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To:      []mail.Address{{Name: "Bob\r\nBcc: attacker@example.net", Email: "bob@example.com"}},
		Subject: "hi",
		Body:    mail.Body{Text: "hello"},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasHeader(raw, "Bcc") {
		t.Errorf("injection through a display name created a real Bcc header:\n%s", raw)
	}
	if len(headerNames(raw)) != 5 { // From, To, Subject, MIME-Version, Content-Type
		t.Errorf("unexpected header set %v:\n%s", headerNames(raw), raw)
	}
}

// A reply without threading headers arrives as a new conversation, which is the most common
// way an otherwise-correct reply looks broken to whoever receives it.
func TestComposeSetsThreadingHeadersOnReply(t *testing.T) {
	raw, err := Compose(
		mail.Outgoing{To: to("bob@example.com"), Subject: "Re: hi", Body: mail.Body{Text: "yes"}},
		"me@example.com",
		&ReplyContext{MessageID: "<abc@example.com>", References: "<older@example.com>"},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if !strings.Contains(got, "In-Reply-To: <abc@example.com>") {
		t.Error("In-Reply-To missing")
	}
	if !strings.Contains(got, "References: <older@example.com> <abc@example.com>") {
		t.Errorf("References should chain the parent onto the existing chain, got:\n%s", got)
	}
}

func TestComposeEncodesNonASCIISubject(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To: to("a@example.com"), Subject: "Grüße — café", Body: mail.Body{Text: "x"},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	if !strings.Contains(headers, "=?utf-8?") {
		t.Fatalf("a non-ASCII subject should be encoded-word encoded, got:\n%s", headers)
	}
}

func TestComposeMultipartWithAttachment(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To:      to("a@example.com"),
		Subject: "report",
		Body:    mail.Body{Text: "see attached"},
		Attachments: []mail.Attachment{{
			AttachmentRef: mail.AttachmentRef{Filename: "report.csv", MimeType: "text/csv"},
			Content:       []byte("a,b,c\n1,2,3\n"),
		}},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{"multipart/mixed", `filename="report.csv"`, "Content-Transfer-Encoding: base64"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in composed message", want)
		}
	}
}

func TestComposeAlternativeWhenBothBodiesPresent(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To:      to("a@example.com"),
		Subject: "hi",
		Body:    mail.Body{Text: "plain", HTML: "<p>rich</p>"},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "multipart/alternative") {
		t.Error("expected multipart/alternative when both text and HTML are supplied")
	}
	if !strings.Contains(got, "plain") || !strings.Contains(got, "<p>rich</p>") {
		t.Error("both alternatives should be present in the body")
	}
}

func TestComposePlainOnlyStaysSinglePart(t *testing.T) {
	raw, err := Compose(mail.Outgoing{
		To: to("a@example.com"), Subject: "hi", Body: mail.Body{Text: "just text"},
	}, "me@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, "multipart") {
		t.Error("a plain-text-only message should not be multipart")
	}
	if !strings.Contains(got, "Content-Type: text/plain; charset=utf-8") {
		t.Errorf("expected a plain text content type, got:\n%s", got)
	}
}
