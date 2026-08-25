package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
)

// oneAttachment is a provider holding a single file, whatever it is asked for.
type oneAttachment struct{ att mail.Attachment }

func (oneAttachment) ID() mail.ProviderID      { return mail.ProviderGmail }
func (o oneAttachment) Capabilities() mail.Set { return mail.DerivedCapabilities(o) }
func (oneAttachment) Quirks() []mail.Quirk     { return nil }

func (o oneAttachment) GetAttachment(context.Context, mail.ScopedID, string) (mail.Attachment, error) {
	return o.att, nil
}

// blobRig gives the tools a real blob store over a real database, because the interesting
// assertions here are about what does and does not end up in the tool result — which depends
// on the store actually having accepted the bytes.
type blobRig struct {
	tools *Tools
	blobs *blob.Store
	db    *store.Store
	owner user.ID
	grant grant.ID
}

func newBlobRig(t *testing.T, p mail.Provider) *blobRig {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open("sqlite://" + filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	dir, err := blob.NewDir(filepath.Join(root, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := blob.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := blob.New(dir, db, signer, "https://mail.example.com", blob.Options{TTL: 15 * time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	u, _, err := db.EnsureUser(context.Background(), user.User{
		Issuer: "https://idp.example.com", Subject: "alice", Email: "alice@example.com",
	}, store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		t.Fatalf("signing in: %v", err)
	}
	if err := db.LinkAccount(context.Background(), u.ID, mail.Account{
		ID: testAccount.ID, Alias: testAccount.Alias, Address: testAccount.Address,
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}, "sealed", ""); err != nil {
		t.Fatalf("linking: %v", err)
	}
	if err := db.RegisterClient(context.Background(), store.Client{
		ID: "client_1", Name: "test", RedirectURIs: []string{"https://client.example/cb"},
	}); err != nil {
		t.Fatal(err)
	}
	g := &grant.Grant{
		ID: "grant_1", OwnerID: u.ID, ClientID: "client_1", Label: "test",
		Accounts: []mail.AccountID{testAccount.ID},
		Caps:     mail.NewSet(mail.CapAttachments, mail.CapDraft, mail.CapSend),
	}
	if err := db.CreateGrant(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	return &blobRig{
		tools: NewTools(grant.NewGate(oneMailbox{}, silentAudit{}, nil), oneProvider{p}, oneMailbox{}).
			WithBlobs(blobs),
		blobs: blobs, db: db, owner: u.ID, grant: g.ID,
	}
}

// ctx builds a request context carrying the rig's real grant, so an owner-scoped blob lookup
// resolves against rows that exist.
func (r *blobRig) ctx(caps ...mail.Capability) context.Context {
	return context.WithValue(context.Background(), grantKey{}, &grant.Grant{
		ID: r.grant, OwnerID: r.owner, Accounts: []mail.AccountID{testAccount.ID},
		Caps: mail.NewSet(caps...),
	})
}

// The whole point of the change: a 5 MB PDF used to be ~6.7 MB of base64 in the model's
// context, where it could not be read in any case.
func TestGetAttachmentAnswersWithALinkAndNotTheFile(t *testing.T) {
	content := []byte("%PDF-1.7\nthe invoice nobody should see in a transcript\n")
	r := newBlobRig(t, oneAttachment{mail.Attachment{
		AttachmentRef: mail.AttachmentRef{ID: "att1", Filename: "invoice.pdf", MimeType: "application/pdf"},
		Content:       content,
	}})

	res, _, err := r.tools.handleGetAttachment(r.ctx(mail.CapAttachments), nil, attachmentArgs{
		MessageID: "acct_1:msg_1", AttachmentID: "att1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("refused: %v", payload(t, res))
	}

	var link *sdk.ResourceLink
	for _, c := range res.Content {
		if rl, ok := c.(*sdk.ResourceLink); ok {
			link = rl
		}
	}
	if link == nil {
		t.Fatal("the result should carry a resource link a client can act on")
	}
	if !strings.HasPrefix(link.URI, "https://mail.example.com/attachments/") {
		t.Errorf("the link should point at this server's attachment route, got %q", link.URI)
	}

	body := payload(t, res)
	for _, field := range []string{"url", "filename", "mime_type", "size_bytes", "expires_at"} {
		if _, ok := body[field]; !ok {
			t.Errorf("the result should carry %s so a caller can decide what to do", field)
		}
	}

	// The real assertion. Nothing anywhere in the result may be the file.
	whole, _ := json.Marshal(res)
	if strings.Contains(string(whole), "the invoice nobody should see") {
		t.Error("the attachment's content reached the conversation")
	}
	if strings.Contains(string(whole), "JVBERi0") {
		t.Error("the attachment reached the conversation base64 encoded")
	}
}

func TestInlineDownloadReturnsSmallTextAsText(t *testing.T) {
	r := newBlobRig(t, oneAttachment{mail.Attachment{
		AttachmentRef: mail.AttachmentRef{ID: "att1", Filename: "notes.txt", MimeType: "text/plain"},
		Content:       []byte("meet at noon"),
	}})

	res, _, err := r.tools.handleGetAttachment(r.ctx(mail.CapAttachments), nil, attachmentArgs{
		MessageID: "acct_1:msg_1", AttachmentID: "att1", Inline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("refused: %v", payload(t, res))
	}
	if got := payload(t, res)["content"]; got != "meet at noon" {
		t.Errorf("want the text itself, got %v", got)
	}
}

// The cap on the one path that still puts content in the caller's context, and the reason it
// has to be a hard number rather than a judgement.
func TestInlineDownloadRefusesAnythingOverTheCap(t *testing.T) {
	r := newBlobRig(t, oneAttachment{mail.Attachment{
		AttachmentRef: mail.AttachmentRef{ID: "att1", Filename: "big.txt", MimeType: "text/plain"},
		Content:       make([]byte, maxInlineDownload+1),
	}})

	res, _, err := r.tools.handleGetAttachment(r.ctx(mail.CapAttachments), nil, attachmentArgs{
		MessageID: "acct_1:msg_1", AttachmentID: "att1", Inline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("an oversized inline read must be refused")
	}
	message, _ := payload(t, res)["message"].(string)
	if !strings.Contains(message, "inline limit") || !strings.Contains(message, "download URL") {
		t.Errorf("the refusal should name the limit and point at the link: %q", message)
	}
}

// Base64 in a conversation is exactly what this change removes, so the inline path refuses
// anything that is not readable text rather than falling back to it.
func TestInlineDownloadRefusesBinary(t *testing.T) {
	r := newBlobRig(t, oneAttachment{mail.Attachment{
		AttachmentRef: mail.AttachmentRef{ID: "att1", Filename: "logo.png", MimeType: "image/png"},
		Content:       []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe},
	}})

	res, _, err := r.tools.handleGetAttachment(r.ctx(mail.CapAttachments), nil, attachmentArgs{
		MessageID: "acct_1:msg_1", AttachmentID: "att1", Inline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("binary content must not be inlined")
	}
	if message, _ := payload(t, res)["message"].(string); !strings.Contains(message, "not text") {
		t.Errorf("the refusal should say why: %q", message)
	}
}

// The tool description is what an agent reads to decide how to behave, and the result is what
// it acts on. Both have to say that the client performs the upload itself.
func TestUploadURLTellsTheClientEverythingItNeeds(t *testing.T) {
	r := newBlobRig(t, oneAttachment{})

	res, _, err := r.tools.handleUploadURL(r.ctx(mail.CapSend), nil, uploadArgs{
		Filename: "report.pdf", MimeType: "application/pdf", SizeBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("refused: %v", payload(t, res))
	}

	body := payload(t, res)
	if body["method"] != "PUT" {
		t.Errorf("the client has to be told the method, got %v", body["method"])
	}
	if url, _ := body["upload_url"].(string); !strings.Contains(url, "/attachments/upload/") {
		t.Errorf("want an upload URL, got %v", body["upload_url"])
	}
	if max, _ := body["max_bytes"].(float64); max != 4096 {
		t.Errorf("a declared size should narrow the ceiling, got %v", body["max_bytes"])
	}
	for _, field := range []string{"blob_id", "expires_at", "how"} {
		if _, ok := body[field]; !ok {
			t.Errorf("the result should carry %s", field)
		}
	}
}

// A declared size only ever narrows: asking for more than a message could carry gets the
// message ceiling, not the number that was asked for.
func TestUploadURLCannotAskForMoreThanAMessageHolds(t *testing.T) {
	r := newBlobRig(t, oneAttachment{})

	res, _, err := r.tools.handleUploadURL(r.ctx(mail.CapDraft), nil, uploadArgs{
		Filename: "huge.bin", SizeBytes: 1 << 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("refused: %v", payload(t, res))
	}
	if max, _ := payload(t, res)["max_bytes"].(float64); int64(max) != mail.MaxAttachmentBytes {
		t.Errorf("want the message ceiling, got %v", max)
	}
}

// The other half of the round trip: bytes the client uploaded reach the provider unchanged,
// having never been in a tool call.
func TestAnUploadedBlobCanBeAttached(t *testing.T) {
	r := newBlobRig(t, oneAttachment{})
	ctx := r.ctx(mail.CapSend)
	g := grantFrom(ctx)

	up, err := r.blobs.NewUpload(ctx, blob.UploadRequest{
		Owner: r.owner, GrantID: r.grant, Filename: "report.pdf", MimeType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.blobs.Receive(ctx, blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: r.owner, Grant: r.grant,
		Expires: time.Now().Add(time.Minute), Max: up.MaxBytes,
	}, strings.NewReader("the report itself")); err != nil {
		t.Fatalf("receiving the upload: %v", err)
	}

	got, err := r.tools.resolveAttachments(ctx, g, []attachmentInput{{BlobID: up.ID}})
	if err != nil {
		t.Fatalf("attaching an upload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one attachment, got %d", len(got))
	}
	if string(got[0].Content) != "the report itself" {
		t.Errorf("the bytes changed on the way through: %q", got[0].Content)
	}
	if got[0].Filename != "report.pdf" || got[0].MimeType != "application/pdf" {
		t.Errorf("the upload's own name and type should carry through: %+v", got[0].AttachmentRef)
	}
}

// A blob belongs to the grant that made it. One client's staged file is not another's to
// send, and it is reported as missing rather than as forbidden — confirming the id exists
// under a different grant would itself be a disclosure.
func TestABlobStagedByAnotherGrantCannotBeAttached(t *testing.T) {
	r := newBlobRig(t, oneAttachment{})
	ctx := r.ctx(mail.CapSend)

	up, err := r.blobs.NewUpload(ctx, blob.UploadRequest{
		Owner: r.owner, GrantID: r.grant, Filename: "report.pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.blobs.Receive(ctx, blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: r.owner, Grant: r.grant,
		Expires: time.Now().Add(time.Minute), Max: up.MaxBytes,
	}, strings.NewReader("private")); err != nil {
		t.Fatal(err)
	}

	other := &grant.Grant{
		ID: "grant_2", OwnerID: r.owner, Accounts: []mail.AccountID{testAccount.ID},
		Caps: mail.NewSet(mail.CapSend),
	}
	_, err = r.tools.resolveAttachments(ctx, other, []attachmentInput{{BlobID: up.ID}})
	if err == nil {
		t.Fatal("a blob from another grant must not be attachable")
	}
	if !strings.Contains(err.Error(), "no such blob") {
		t.Errorf("want a not-found refusal, got %v", err)
	}
}

// Exactly one source. Adding a third way to supply an attachment is where a pairwise check
// quietly stops covering every pair.
func TestAnAttachmentMayNameOnlyOneSource(t *testing.T) {
	tools := &Tools{}
	_, err := tools.resolveAttachments(context.Background(), &grant.Grant{}, []attachmentInput{{
		Filename: "x.txt", BlobID: "blob_1", FromMessage: "acct_1:abc", AttachmentID: "att1",
	}})
	if err == nil || !strings.Contains(err.Error(), "more than one source") {
		t.Fatalf("want a refusal naming the ambiguity, got %v", err)
	}
}
