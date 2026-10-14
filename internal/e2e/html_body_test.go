package e2e

import (
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// withBody replaces a seeded message's body, so a read path has HTML to find.
func (m *mailbox) withBody(id mail.ScopedID, body mail.Body) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg := m.messages[id.Native]
	msg.Body = body
	m.messages[id.Native] = msg
}

// The whole path a client takes to a link: a real token, the real MCP transport, and the
// argument as the generated schema accepts it. The case is the one that prompted the feature —
// a text part that keeps a button's words and drops its address.
func TestAClientGetsTheLinksAnHTMLMessageCarries(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	box := r.mailbox(work)
	msg := box.seed("Change of ownership", "a.txt", "text/plain", []byte("x"))
	const form = "https://forms.example.com/ownership?id=1&ref=2"
	box.withBody(msg, mail.Body{
		Text: "Please complete our: [Change of Ownership form]",
		HTML: `<p>Please complete our: <a href="https://forms.example.com/ownership?id=1&amp;ref=2">Change of Ownership form</a></p>`,
	})

	s, _ := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work}, caps: []mail.Capability{mail.CapRead},
	})

	res := s.callOK("mail_get_message", map[string]any{"id": msg.String()})
	links, _ := res.payload["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("want one link, got:\n%s", res.text)
	}
	link, _ := links[0].(map[string]any)
	if link["url"] != form || link["text"] != "Change of Ownership form" {
		t.Errorf("link = %v, want the form under its own name", link)
	}
	if _, ok := res.payload["html"]; ok {
		t.Error("html must be absent unless asked for")
	}

	res = s.callOK("mail_get_message", map[string]any{"id": msg.String(), "include_html": true})
	if html, _ := res.payload["html"].(string); !strings.Contains(html, "forms.example.com") {
		t.Errorf("include_html did not return the HTML:\n%s", res.text)
	}

	res = s.callOK("mail_get_thread", map[string]any{"id": msg.String()})
	messages, _ := res.payload["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("thread came back empty:\n%s", res.text)
	}
	first, _ := messages[0].(map[string]any)
	if links, _ := first["links"].([]any); len(links) != 1 {
		t.Errorf("a thread message must carry its links too:\n%s", res.text)
	}
}

// The tool descriptions are where an agent learns these fields exist.
func TestTheReadToolsAdvertiseLinksAndHTML(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work}, caps: []mail.Capability{mail.CapRead},
	})
	tools := s.tools()
	for _, name := range []string{"mail_get_message", "mail_get_thread"} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("%s is not offered", name)
		}
		if !strings.Contains(tool.Description, "links") {
			t.Errorf("%s does not mention links: %q", name, tool.Description)
		}
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["include_html"]; !ok {
			t.Errorf("%s schema has no include_html: %v", name, tool.InputSchema)
		}
	}
}
