package e2e

import (
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/mail"
)

// What a client is actually told, driven over the real transport.
//
// internal/mcp asserts the table; this asserts the wire. Annotations are attached at
// registration, marshalled by the SDK and read back by a client through tools/list, and a hint
// that is right in the table and absent from the response is a hint nothing acts on — the
// pointer-valued fields in particular are omitempty, so a nil is silence rather than a false.

func (s *session) prompts() []*sdk.Prompt {
	s.t.Helper()
	res, err := s.cs.ListPrompts(s.ctx, &sdk.ListPromptsParams{})
	if err != nil {
		s.t.Fatalf("prompts/list: %v", err)
	}
	return res.Prompts
}

func (s *session) prompt(name string) string {
	s.t.Helper()
	res, err := s.cs.GetPrompt(s.ctx, &sdk.GetPromptParams{Name: name})
	if err != nil {
		s.t.Fatalf("prompts/get %s: %v", name, err)
	}
	var b strings.Builder
	for _, m := range res.Messages {
		if text, ok := m.Content.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// TestAnnotationsSurviveToolsList checks the hints a client would base a policy on, on the
// four tools where being wrong costs the most: the read it may run unattended, the send it
// must not, the delete it must confirm, and the one write whose world is closed.
func TestAnnotationsSurviveToolsList(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Everything", accounts: []mail.Account{work}, caps: mail.AllCapabilities,
	})

	tools := s.tools()
	for name, want := range map[string]struct {
		title       string
		readOnly    bool
		destructive bool
		idempotent  bool
		openWorld   bool
	}{
		"mail_search":     {"Search mail", true, false, true, true},
		"mail_send":       {"Send mail", false, true, false, true},
		"mail_trash":      {"Trash, restore or delete permanently", false, true, false, true},
		"mail_upload_url": {"Stage a file to attach", false, false, false, false},
	} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("%s was not offered", name)
		}
		a := tool.Annotations
		if a == nil {
			t.Fatalf("%s arrived with no annotations", name)
		}
		if a.Title != want.title {
			t.Errorf("%s: title %q, want %q", name, a.Title, want.title)
		}
		if a.ReadOnlyHint != want.readOnly {
			t.Errorf("%s: readOnlyHint %v, want %v", name, a.ReadOnlyHint, want.readOnly)
		}
		if a.DestructiveHint == nil {
			t.Errorf("%s: destructiveHint did not survive the wire", name)
		} else if *a.DestructiveHint != want.destructive {
			t.Errorf("%s: destructiveHint %v, want %v", name, *a.DestructiveHint, want.destructive)
		}
		if a.IdempotentHint != want.idempotent {
			t.Errorf("%s: idempotentHint %v, want %v", name, a.IdempotentHint, want.idempotent)
		}
		if a.OpenWorldHint == nil {
			t.Errorf("%s: openWorldHint did not survive the wire", name)
		} else if *a.OpenWorldHint != want.openWorld {
			t.Errorf("%s: openWorldHint %v, want %v", name, *a.OpenWorldHint, want.openWorld)
		}
	}

	// Nothing is offered unannotated, which is the claim a client's policy engine depends on:
	// a missing hint falls back to a default that may be wrong in either direction.
	for name, tool := range tools {
		if tool.Annotations == nil {
			t.Errorf("%s reached a client with no annotations", name)
		}
	}
}

// TestTheAttachmentPromptIsListedAndRetrievable. A prompt that is registered but not
// advertised, or advertised and not fetchable, is worth nothing — and both are server
// capability negotiation rather than anything this code says, so they are driven rather than
// reasoned about.
func TestTheAttachmentPromptIsListedAndRetrievable(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Attacher", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments, mail.CapSend},
	})

	listed := s.prompts()
	if len(listed) != 1 || listed[0].Name != "mail_attachments" {
		t.Fatalf("prompts/list answered %+v", listed)
	}
	if listed[0].Title == "" || listed[0].Description == "" {
		t.Errorf("the prompt is listed with nothing to decide by: %+v", listed[0])
	}

	text := s.prompt("mail_attachments")
	for _, want := range []string{"mail_upload_url", "PUT", "blob_id", "mail_get_attachment"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt never mentions %q:\n%s", want, text)
		}
	}

	// A grant with neither attachment tool is offered no attachment prompt, for the reason its
	// tools are filtered: instructions for a call it cannot make are context spent for nothing.
	plain, _ := r.grantFor(approval{
		label: "Reader only", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead},
	})
	if got := plain.prompts(); len(got) != 0 {
		t.Errorf("a read-only grant was offered %+v", got)
	}
}

// TestAnExpiredBlobIdSaysToUploadAgain drives the whole thing: stage a file, let it expire,
// and name it in a send — once before the sweeper has been round and once after.
//
// Both are real, and they reach the server as different errors: a row past its expiry is
// dropped on read, while a swept one is simply not there. The refusal used to be "attachment
// 1: blob has expired" and "attachment 1: no such blob", which are true and leave an agent
// with nowhere to go — the first reads as though the file it holds is gone, and the second as
// though it mistyped. What it needs is that the staged copy went and that three steps put it
// back.
func TestAnExpiredBlobIdSaysToUploadAgain(t *testing.T) {
	// Long enough that the upload URL is still live when the PUT lands: a signed token carries
	// whole seconds, so a sub-second TTL can round down to an expiry already in the past.
	const ttl = 1500 * time.Millisecond

	for _, sweep := range []bool{false, true} {
		name := "before the sweep"
		if sweep {
			name = "after the sweep"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, options{attachmentTTL: ttl})
			work := r.link("work", "ada@work.example")
			s, _ := r.grantFor(approval{
				label: "Composer", accounts: []mail.Account{work},
				caps: []mail.Capability{mail.CapSend},
			})

			minted := s.callOK("mail_upload_url", map[string]any{"filename": "contract.pdf"})
			blobID := str(minted.payload["blob_id"])
			if status, body := r.put(str(minted.payload["upload_url"]), []byte("%PDF-1.4")); status != 201 {
				t.Fatalf("the upload answered %d: %s", status, body)
			}

			time.Sleep(ttl + 200*time.Millisecond)
			if sweep {
				if _, err := r.blobs.Sweep(r.ctx); err != nil {
					t.Fatalf("sweeping: %v", err)
				}
			}

			refused := s.callError("mail_send", map[string]any{
				"account": "work", "to": []map[string]any{{"email": "legal@example.net"}},
				"subject": "the contract", "attachments": []map[string]any{{"blob_id": blobID}},
			})
			for _, want := range []string{blobID, "expired", "mail_upload_url", "new blob_id"} {
				if !strings.Contains(refused.text, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, refused.text)
				}
			}
			if len(r.mailbox(work).sentMessages()) != 0 {
				t.Fatal("a message went out with an attachment that had expired")
			}
		})
	}
}

// TestBothAttachmentToolsNameTheNoHTTPPath.
//
// MCP promises nothing about a client being able to make its own HTTP requests, and both of
// these tools now assume it can. An agent that cannot has a correct move — hand the URL to the
// person it is working with — and it has to be named rather than inferred, or the agent
// reports that the file could not be retrieved.
func TestBothAttachmentToolsNameTheNoHTTPPath(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Attacher", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments, mail.CapSend},
	})

	tools := s.tools()
	for _, name := range []string{"mail_get_attachment", "mail_upload_url"} {
		description := tools[name].Description
		if !strings.Contains(description, "cannot make HTTP requests") {
			t.Errorf("%s does not say what to do without an HTTP client:\n%s", name, description)
		}
		if !strings.Contains(description, "person you are working with") {
			t.Errorf("%s does not name handing the URL to a person as an outcome:\n%s",
				name, description)
		}
	}
}
