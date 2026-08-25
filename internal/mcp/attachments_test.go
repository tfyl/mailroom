package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

func inline(name, content string) attachmentInput {
	return attachmentInput{
		Filename:      name,
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(content)),
	}
}

func TestInlineAttachmentDecodes(t *testing.T) {
	tools := &Tools{}
	got, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{inline("notes.txt", "hello attachment")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one attachment, got %d", len(got))
	}
	if string(got[0].Content) != "hello attachment" {
		t.Errorf("content did not round trip: %q", got[0].Content)
	}
	if got[0].Filename != "notes.txt" {
		t.Errorf("filename lost: %q", got[0].Filename)
	}
	// A caller that does not say gets a type that means "unknown bytes", not an empty header.
	if got[0].MimeType != "application/octet-stream" {
		t.Errorf("want a default mime type, got %q", got[0].MimeType)
	}
}

func TestInlineAttachmentAcceptsUnpaddedBase64(t *testing.T) {
	tools := &Tools{}
	raw := base64.RawStdEncoding.EncodeToString([]byte("unpadded"))
	got, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{Filename: "a.txt", ContentBase64: raw}})
	if err != nil {
		t.Fatalf("unpadded base64 should be accepted: %v", err)
	}
	if string(got[0].Content) != "unpadded" {
		t.Errorf("got %q", got[0].Content)
	}
}

// The two forms are mutually exclusive. Silently preferring one would mean a caller who
// supplied both gets a message containing something they did not expect.
func TestAttachmentRefusesAmbiguousSource(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{}, []attachmentInput{{
		Filename:      "x.txt",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")),
		FromMessage:   "acct_1:abc",
		AttachmentID:  "att1",
	}})
	if err == nil || !strings.Contains(err.Error(), "one or the other") {
		t.Fatalf("want a refusal naming both sources, got %v", err)
	}
}

func TestAttachmentRefusesEmptySource(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{Filename: "x.txt"}})
	if err == nil || !strings.Contains(err.Error(), "no content") {
		t.Fatalf("want a refusal for an attachment with no content, got %v", err)
	}
}

func TestInlineAttachmentNeedsFilename(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{ContentBase64: base64.StdEncoding.EncodeToString([]byte("x"))}})
	if err == nil || !strings.Contains(err.Error(), "filename") {
		t.Fatalf("want a refusal for a nameless inline attachment, got %v", err)
	}
}

func TestInlineAttachmentRefusesInvalidBase64(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{Filename: "x.bin", ContentBase64: "!!!! not base64 !!!!"}})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("want a refusal naming base64, got %v", err)
	}
}

// Oversized inline content is refused before it is decoded, so an enormous payload does not
// get allocated just to be rejected.
func TestInlineAttachmentSizeLimit(t *testing.T) {
	tools := &Tools{}
	oversized := base64.StdEncoding.EncodeToString(make([]byte, maxInlineAttachment+1))

	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{Filename: "big.bin", ContentBase64: oversized}})
	if err == nil {
		t.Fatal("an oversized inline attachment must be refused")
	}
	if !strings.Contains(err.Error(), "inline limit") {
		t.Errorf("the error should name the limit, got: %v", err)
	}
	// It should point at the cheaper path rather than just saying no.
	if !strings.Contains(err.Error(), "from_message") {
		t.Errorf("the error should suggest referencing instead, got: %v", err)
	}
}

func TestAttachmentsTotalLimit(t *testing.T) {
	tools := &Tools{}
	// Each is under the inline limit; together they exceed the per-message total.
	chunk := base64.StdEncoding.EncodeToString(make([]byte, maxInlineAttachment))
	var inputs []attachmentInput
	for i := 0; i < (maxTotalAttachments/maxInlineAttachment)+1; i++ {
		inputs = append(inputs, attachmentInput{Filename: "chunk.bin", ContentBase64: chunk})
	}

	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{}, inputs)
	if err == nil {
		t.Fatal("attachments summing over the total must be refused")
	}
	if !strings.Contains(err.Error(), "total") {
		t.Errorf("the error should name the total, got: %v", err)
	}
}

func TestNoAttachmentsIsNotAnError(t *testing.T) {
	tools := &Tools{}
	got, err := tools.resolveAttachments(context.Background(), &grant.Grant{}, nil)
	if err != nil || got != nil {
		t.Fatalf("an empty list should resolve to nothing, got %v / %v", got, err)
	}
}

// A reference names the mailbox in its own id, so a malformed one is refused rather than
// being resolved against the composing mailbox.
func TestReferenceRequiresAWellFormedID(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{FromMessage: "not-a-scoped-id", AttachmentID: "att1"}})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want a malformed-id refusal, got %v", err)
	}
}

func TestReferenceRequiresAnAttachmentID(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{},
		[]attachmentInput{{FromMessage: "acct_1:abc"}})
	if err == nil || !strings.Contains(err.Error(), "attachment_id") {
		t.Fatalf("want a refusal naming attachment_id, got %v", err)
	}
}

// The composeArgs carry attachments through to the provider unchanged.
func TestOutgoingCarriesAttachments(t *testing.T) {
	tools := &Tools{}
	att := []mail.Attachment{{
		AttachmentRef: mail.AttachmentRef{Filename: "a.pdf", MimeType: "application/pdf"},
		Content:       []byte("pdf bytes"),
	}}

	out := tools.outgoing(mail.Account{ID: "acct_1"}, composeArgs{Subject: "hi"}, mail.ScopedID{}, att)
	if len(out.Attachments) != 1 || out.Attachments[0].Filename != "a.pdf" {
		t.Fatalf("attachments did not reach Outgoing: %+v", out.Attachments)
	}
}

// A blob_id that does not resolve, and what the caller is told to do about it.
//
// The sentinels underneath are accurate and useless on their own: "blob has expired" and "no
// such blob" both read as "the file is gone", and the response to each is the same three steps
// — mint another URL, PUT the bytes, name the new id — which nothing used to mention. The
// sentinel stays wrapped so errors.Is and the audit row are unaffected; what is asserted here
// is the sentence the model reads.
func TestAStagedBlobFailureSaysWhatToDoNext(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		wants []string
	}{
		{
			name: "expired",
			err:  blob.ErrGone,
			// Expiry is the common way to reach this in a long conversation, so it says the
			// bytes went rather than that the id was wrong — a caller told "not found" goes
			// hunting for a typo.
			wants: []string{"expired", "mail_upload_url", "PUT", "new blob_id"},
		},
		{
			name:  "never written",
			err:   blob.ErrNotReady,
			wants: []string{"no bytes ever", "PUT"},
		},
		{
			name: "unknown",
			err:  blob.ErrNotFound,
			// Once a blob has been swept, an expired id and a wrong one are the same
			// row-shaped absence, so this leads with the likelier of the two rather than
			// with "not found".
			wants: []string{"expired", "mail_upload_url", "new blob_id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := stagedBlobError(0, "blob_abc123", tc.err)
			if !errors.Is(err, tc.err) {
				t.Fatalf("the sentinel was lost: %v", err)
			}
			text := err.Error()
			if !strings.Contains(text, "attachment 1") {
				t.Errorf("the refusal does not say which attachment failed: %s", text)
			}
			if !strings.Contains(text, "blob_abc123") {
				t.Errorf("the refusal does not name the blob_id: %s", text)
			}
			for _, want := range tc.wants {
				if !strings.Contains(text, want) {
					t.Errorf("the refusal does not say %q: %s", want, text)
				}
			}
		})
	}
}

// An error that is none of the three keeps its own words. Rewriting an unrecognised failure
// into upload advice would be the same mistake in the other direction.
func TestAnUnrecognisedBlobFailureIsPassedThrough(t *testing.T) {
	err := stagedBlobError(2, "blob_xyz", errors.New("the disk is on fire"))
	if !strings.Contains(err.Error(), "the disk is on fire") {
		t.Errorf("the underlying error was replaced: %s", err)
	}
	if !strings.Contains(err.Error(), "attachment 3") {
		t.Errorf("the refusal does not say which attachment failed: %s", err)
	}
}
