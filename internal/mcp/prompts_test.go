package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

func promptNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	names := make([]string, 0, len(res.Prompts))
	for _, p := range res.Prompts {
		names = append(names, p.Name)
	}
	return names
}

func promptText(t *testing.T, s *mcp.ClientSession, name string) string {
	t.Helper()
	res, err := s.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name})
	if err != nil {
		t.Fatalf("prompts/get %s: %v", name, err)
	}
	var b strings.Builder
	for _, m := range res.Messages {
		if text, ok := m.Content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// The prompt exists to explain the step no tool owns: the HTTP request the client makes for
// itself, between minting a URL and naming the blob_id. If it stops saying that, it has
// stopped being worth having.
func TestAttachmentPromptDescribesTheWholeSequence(t *testing.T) {
	session, err := connect(t, readGrant(mail.CapRead, mail.CapAttachments, mail.CapSend))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	names := promptNames(t, session)
	if len(names) != 1 || names[0] != attachmentsPrompt {
		t.Fatalf("prompts/list answered %v", names)
	}

	text := promptText(t, session, attachmentsPrompt)
	for _, want := range []string{
		"mail_get_message", // where an attachment id comes from
		"mail_get_attachment",
		"mail_upload_url",
		"PUT",     // the step the client performs itself
		"blob_id", // what the sequence ends in
		"mail_send",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the attachment prompt never mentions %q:\n%s", want, text)
		}
	}
}

// A prompt naming tools the grant does not hold is context spent on an action the client
// cannot take, which is the reason tools are filtered to the grant in the first place.
func TestTheAttachmentPromptIsCutToTheGrant(t *testing.T) {
	cases := []struct {
		name    string
		caps    []mail.Capability
		offered bool
		wants   []string
		avoids  []string
	}{
		{
			name:    "attachments only",
			caps:    []mail.Capability{mail.CapRead, mail.CapAttachments},
			offered: true,
			wants:   []string{"mail_get_attachment"},
			avoids:  []string{"mail_upload_url"},
		},
		{
			name:    "compose only",
			caps:    []mail.Capability{mail.CapDraft},
			offered: true,
			wants:   []string{"mail_upload_url"},
			avoids:  []string{"mail_get_attachment"},
		},
		{
			name:    "neither",
			caps:    []mail.Capability{mail.CapRead, mail.CapLabels},
			offered: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, err := connect(t, readGrant(tc.caps...))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			names := promptNames(t, session)
			if !tc.offered {
				if len(names) != 0 {
					t.Fatalf("a grant that can neither download nor attach was offered %v", names)
				}
				return
			}
			if len(names) != 1 {
				t.Fatalf("prompts/list answered %v", names)
			}
			text := promptText(t, session, attachmentsPrompt)
			for _, want := range tc.wants {
				if !strings.Contains(text, want) {
					t.Errorf("the prompt does not mention %q:\n%s", want, text)
				}
			}
			for _, avoid := range tc.avoids {
				if strings.Contains(text, avoid) {
					t.Errorf("the prompt mentions %q, which this grant cannot call:\n%s", avoid, text)
				}
			}
		})
	}
}

// The prompt says what to do when the client cannot make HTTP requests of its own, because MCP
// promises no such thing and both attachment tools assume it. Handing the URL to a person is a
// finished job, and has to be named as one somewhere.
func TestTheAttachmentPromptNamesTheNoHTTPPath(t *testing.T) {
	session, err := connect(t, readGrant(mail.CapAttachments, mail.CapSend))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	text := promptText(t, session, attachmentsPrompt)
	if !strings.Contains(text, "cannot make HTTP requests") {
		t.Errorf("the prompt does not say what an agent with no HTTP client should do:\n%s", text)
	}
	if !strings.Contains(text, "content_base64") {
		t.Errorf("the prompt does not name the inline fallback for a small file:\n%s", text)
	}
}

// The mode does not change the attachment workflow — nothing on either tool is held — so the
// prompt reads the same whichever mode a grant carries. Asserted rather than assumed, because
// a mode-dependent prompt would be a second place for the hold wording to drift out of step
// with the tool descriptions.
func TestTheAttachmentPromptDoesNotVaryByMode(t *testing.T) {
	var first string
	for _, mode := range []grant.Mode{grant.ModeUnattended, grant.ModeConfirm, grant.ModeHold} {
		g := readGrant(mail.CapAttachments, mail.CapSend)
		g.Mode = mode
		session, err := connect(t, g)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		text := promptText(t, session, attachmentsPrompt)
		if first == "" {
			first = text
			continue
		}
		if text != first {
			t.Errorf("the attachment prompt differs under %s", mode)
		}
	}
}
