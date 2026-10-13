package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// oneMessage answers every read with the same message, and a thread of it twice.
type oneMessage struct{ body mail.Body }

func (oneMessage) ID() mail.ProviderID      { return mail.ProviderGmail }
func (o oneMessage) Capabilities() mail.Set { return mail.DerivedCapabilities(o) }
func (oneMessage) Quirks() []mail.Quirk     { return nil }

func (oneMessage) Search(context.Context, mail.Query, string) (mail.Page[mail.Message], error) {
	return mail.Page[mail.Message]{}, nil
}

func (o oneMessage) Get(_ context.Context, id mail.ScopedID) (mail.Message, error) {
	return mail.Message{ID: id, ThreadID: id, Account: "work", Body: o.body}, nil
}

func (o oneMessage) GetThread(ctx context.Context, id mail.ScopedID) (mail.Thread, error) {
	m, _ := o.Get(ctx, id)
	return mail.Thread{ID: id, Account: "work", Messages: []mail.Message{m, m}}, nil
}

const formHTML = `<html><head><style>p{margin:0}</style></head><body>
<p>Please complete the <a href="https://forms.example.com/ownership?ref=1">Change of Ownership form</a>.</p>
<p><a href="https://example.com/help">Help</a> | <a href="https://forms.example.com/ownership?ref=1">here</a></p>
</body></html>`

func getMessage(t *testing.T, body mail.Body, includeHTML bool) map[string]any {
	t.Helper()
	res, _, err := toolsFor(oneMessage{body}).handleGetMessage(grantWith(mail.CapRead), nil,
		idArgs{ID: "acct_1:m1", IncludeHTML: includeHTML})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_message failed: %+v", res.Content)
	}
	return payload(t, res)
}

func linkList(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["links"].([]any)
	if !ok {
		t.Fatalf("links missing or not a list: %#v", out["links"])
	}
	links := make([]map[string]any, len(raw))
	for i, l := range raw {
		links[i] = l.(map[string]any)
	}
	return links
}

// The case that prompted this: the text part keeps a button's words and drops its address.
// The text stays the body, and the address comes back under links.
func TestLinksComeFromTheHTMLWhenTheTextPartDroppedThem(t *testing.T) {
	out := getMessage(t, mail.Body{Text: "Please complete the [Change of Ownership form].", HTML: formHTML}, false)

	if out["body"] != "Please complete the [Change of Ownership form]." || out["body_source"] != "text" {
		t.Errorf("the text part must stay the body: body=%q source=%v", out["body"], out["body_source"])
	}
	links := linkList(t, out)
	if len(links) != 2 {
		t.Fatalf("want two distinct targets, got %v", links)
	}
	if links[0]["text"] != "Change of Ownership form" || links[0]["url"] != "https://forms.example.com/ownership?ref=1" {
		t.Errorf("first link = %v", links[0])
	}
	if out["has_html"] != true {
		t.Error("has_html must say there is HTML to ask for")
	}
	if _, ok := out["html"]; ok {
		t.Error("html is opt-in and must be absent unless asked for")
	}
}

// With no text part the body used to be the raw HTML, stylesheet first.
func TestAnHTMLOnlyMessageIsRenderedAsText(t *testing.T) {
	out := getMessage(t, mail.Body{HTML: formHTML}, false)

	body, _ := out["body"].(string)
	if out["body_source"] != "html" {
		t.Errorf("body_source = %v, want html", out["body_source"])
	}
	if strings.Contains(body, "<") || strings.Contains(body, "margin") {
		t.Errorf("markup leaked into the body: %q", body)
	}
	if !strings.Contains(body, "Change of Ownership form (https://forms.example.com/ownership?ref=1)") {
		t.Errorf("the link must be written inline after its words: %q", body)
	}
}

func TestPlainTextMailHasAnEmptyLinkList(t *testing.T) {
	out := getMessage(t, mail.Body{Text: "hello"}, true)
	if links := linkList(t, out); len(links) != 0 {
		t.Errorf("links = %v, want none", links)
	}
	if out["has_html"] != false {
		t.Error("has_html must be false with no HTML part")
	}
	if _, ok := out["html"]; ok {
		t.Error("asking for HTML a message does not have must not invent an html field")
	}
}

func TestHTMLIsReturnedOnRequestAndCapped(t *testing.T) {
	out := getMessage(t, mail.Body{HTML: formHTML}, true)
	if out["html"] != formHTML {
		t.Errorf("html = %q, want the part unchanged", out["html"])
	}
	if _, ok := out["html_truncated"]; ok {
		t.Error("a small part must not be marked truncated")
	}

	// A multi-byte character straddling the cut must not be split.
	big := formHTML + strings.Repeat("é", maxHTMLBytes)
	out = getMessage(t, mail.Body{HTML: big}, true)
	html, _ := out["html"].(string)
	if len(html) > maxHTMLBytes || strings.ContainsRune(html, '�') {
		t.Errorf("html is %d bytes or carries a split character", len(html))
	}
	if out["html_truncated"] != float64(len(big)) {
		t.Errorf("html_truncated = %v, want the full size %d", out["html_truncated"], len(big))
	}
	// Links come from the whole document, not the truncated copy.
	if len(linkList(t, out)) != 2 {
		t.Error("truncating html must not lose links")
	}
}

func TestEveryMessageInAThreadCarriesItsLinks(t *testing.T) {
	res, _, err := toolsFor(oneMessage{mail.Body{HTML: formHTML}}).handleGetThread(grantWith(mail.CapRead), nil,
		idArgs{ID: "acct_1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := payload(t, res)["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("want two messages, got %d", len(messages))
	}
	for i, m := range messages {
		if links := linkList(t, m.(map[string]any)); len(links) != 2 {
			t.Errorf("message %d links = %v", i, links)
		}
	}
}
