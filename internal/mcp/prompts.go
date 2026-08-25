package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// Prompts, and why this server has exactly one.
//
// A tool description is read in the middle of a decision about that tool. It is the wrong
// place to explain a sequence, because the sequence spans three things and only one of them
// is a tool call: mail_upload_url mints a URL, the *client* performs an HTTP PUT, and then
// mail_send names the blob_id. The middle step is the one nothing else on this server can
// describe — no tool has it in scope, and a model that misses it holds a blob_id pointing at
// an empty reservation and cannot tell why the attachment is missing.
//
// That is the whole test for whether a prompt earns its place: it explains something no single
// tool owns. Attachments do. Nothing else here does — searching, reading and sending are each
// one call, and their descriptions already say when to make it — so this is the only prompt,
// and adding a second one that restated tool descriptions would make this one harder to find.
//
// It is offered only to a grant that has at least one of the attachment tools, for the same
// reason tools are filtered to the grant: instructions for a capability the client does not
// hold are context spent on an action it cannot take.

const attachmentsPrompt = "mail_attachments"

// registerPrompts adds the prompts this grant can act on.
func registerPrompts(srv *mcp.Server, g *grant.Grant) {
	canDownload := g.Caps.Has(mail.CapAttachments)
	canUpload := g.Caps.Has(mail.CapDraft) || g.Caps.Has(mail.CapSend)
	if !canDownload && !canUpload {
		return
	}

	srv.AddPrompt(&mcp.Prompt{
		Name:  attachmentsPrompt,
		Title: "Working with attachments",
		Description: "How files move in and out of a mailbox through this server: what to call, " +
			"what to fetch or upload yourself, and what to do when you cannot make HTTP requests.",
	}, func(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "The attachment workflow, end to end.",
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: attachmentGuidance(canDownload, canUpload)},
			}},
		}, nil
	})
}

// attachmentGuidance is the text of the prompt, cut to the halves this grant can perform.
//
// Written as ordered steps rather than as advice, on the same rule the tool descriptions
// follow: an instruction naming the act and its order is one a model either followed or did
// not, while "handle attachments carefully" is neither.
func attachmentGuidance(canDownload, canUpload bool) string {
	text := "Attachment bytes never travel through this conversation. Every file crosses as a " +
		"short-lived signed URL that you fetch or write to with your own HTTP client, so the " +
		"steps below include work that is yours to do rather than a tool call.\n"

	if canDownload {
		text += "\nTo get a file out of a mailbox:\n" +
			"1. Find the message and read its attachment manifest with mail_get_message. Each " +
			"entry has an `id`, a filename, a media type and a size.\n" +
			"2. Call mail_get_attachment with the message id and that attachment id. It answers " +
			"with `url`, `size_bytes` and `expires_at` — not the file.\n" +
			"3. GET the url yourself. Send no Authorization header: the signature is in the " +
			"URL. Do it now rather than later; the link expires within minutes and dies " +
			"immediately if the grant behind it is revoked or narrowed.\n" +
			"4. If you cannot make HTTP requests, give the url to the person you are working " +
			"with along with the filename and the expiry, and say that fetching it is theirs " +
			"to do. That is a completed task, not a failure — do not report the file as " +
			"unavailable. For a small text file whose actual wording you need, call " +
			"mail_get_attachment again with `inline: true` instead, which returns the text and " +
			"refuses anything large or non-text.\n"
	}

	if canUpload {
		text += "\nTo attach a file to a message:\n" +
			"1. Call mail_upload_url with the filename the recipient should see, and " +
			"`size_bytes` if you know it. It answers with `upload_url`, `blob_id`, " +
			"`max_bytes`, `expires_at` (the URL's) and `blob_expires_at` (the staged file's).\n" +
			"2. PUT the file's bytes to `upload_url` as the raw request body, with no " +
			"Authorization header. The URL accepts exactly one PUT and stops working at " +
			"`expires_at`. A 201 means the bytes are staged.\n" +
			"3. Pass {\"blob_id\": \"<the id>\"} in the `attachments` list of mail_draft or " +
			"mail_send, before `blob_expires_at`.\n" +
			"4. If a URL has expired or has already been used, call mail_upload_url again for " +
			"a fresh one. Nothing is recoverable from the old one, and there is no way to " +
			"resume a partial upload.\n" +
			"5. If you cannot make HTTP requests, do not call mail_upload_url and then stop " +
			"with a blob_id nothing was written to. Either attach the file inline as " +
			"`content_base64` — that path is capped at 2 MiB and needs you to hold the bytes " +
			"already — or hand `upload_url` to the person you are working with, ask them to " +
			"PUT the file to it, and wait for them to confirm before you name the blob_id.\n"
	}

	text += "\nTwo things that are easy to get wrong. A blob_id is not the file: naming one " +
		"whose bytes were never PUT, or whose staging window has passed, fails at send time " +
		"rather than at upload time. And a signed URL is a credential — it serves the file to " +
		"anyone holding it with no token at all — so pass one only to the person whose mailbox " +
		"it came from, and do not save it anywhere it will outlive the fetch."

	return text
}
