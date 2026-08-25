package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// Attachments crossed with revocation, removal and the held queue.
//
// A signed URL is a credential that leaves this server and comes back with no bearer token
// on it, so the only thing standing between it and somebody's mail is a database read taken
// on every fetch. These drive that read through the real route.

// TestSignedLinkIsRefusedOnceTheGrantIsRevoked, and once it is removed.
//
// Three states, in the order an operator reaches them: live, revoked, removed. The third is
// the one worth driving rather than reasoning about — a removed grant is loaded by nothing,
// so "the fetch re-reads the grant" has to mean something specific about a grant that is not
// there.
func TestSignedLinkIsRefusedOnceTheGrantIsRevoked(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	content := []byte("the quarterly numbers")
	msg := r.mailbox(work).seed("quarterlies", "q3.txt", "text/plain", content)

	s, id := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments},
	})

	res := s.callOK("mail_get_attachment", map[string]any{
		"message_id": msg.String(), "attachment_id": "att1",
	})
	link := str(res.payload["url"])
	if link == "" {
		t.Fatalf("no download url came back:\n%s", res.text)
	}

	status, body, headers := r.fetch(link)
	if status != http.StatusOK {
		t.Fatalf("the fresh link answered %d: %s", status, body)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("the link served %q", body)
	}
	if got := headers.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition was %q; a blob served inline in this origin is script "+
			"beside the operator's session cookie", got)
	}
	if got := headers.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options was %q", got)
	}

	r.revoke(id)
	status, body, _ = r.fetch(link)
	if status != http.StatusForbidden {
		t.Fatalf("a revoked grant's link answered %d: %s", status, body)
	}

	r.removeGrant(id)
	status, body, _ = r.fetch(link)
	if status == http.StatusOK {
		t.Fatalf("a removed grant's link still served the file: %s", body)
	}
	// Both refusals are correct; which one arrives says which check caught it. Removing a
	// grant expires its blobs in the same transaction, so the row is gone before the grant
	// lookup is reached, and the holder is told the link expired rather than that it was
	// refused. Either way no byte leaves.
	t.Logf("after removal the link answered %d (%s)", status, strings.TrimSpace(string(body)))
	if status != http.StatusGone && status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("an unexpected status for a removed grant's link: %d", status)
	}
}

// TestNarrowingAGrantKillsItsLinks checks the claim internal/blob makes in as many words:
// "Narrowing counts as revoking."
func TestNarrowingAGrantKillsItsLinks(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("quarterlies", "q3.txt", "text/plain", []byte("numbers"))

	s, id := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments},
	})
	res := s.callOK("mail_get_attachment", map[string]any{
		"message_id": msg.String(), "attachment_id": "att1",
	})
	link := str(res.payload["url"])
	if status, _, _ := r.fetch(link); status != http.StatusOK {
		t.Fatalf("the fresh link answered %d", status)
	}

	// Drop `attachments`, keeping the grant alive and its token working.
	if err := r.db.EditGrant(r.ctx, r.owner.ID, id,
		[]mail.AccountID{work.ID}, mail.NewSet(mail.CapRead), grant.ModeConfirm, nil); err != nil {
		t.Fatalf("narrowing the grant: %v", err)
	}
	if status, body, _ := r.fetch(link); status != http.StatusForbidden {
		t.Fatalf("a link outlived the capability behind it: %d %s", status, body)
	}
}

// TestUploadedBlobIsReachableOnlyByTheGrantThatStagedIt.
//
// Two grants, one owner. A blob is the owner's bytes as far as the catalog is concerned, so
// the check that keeps one client's staged file away from another is the grant id on the row,
// and it is worth driving rather than assuming.
func TestUploadedBlobIsReachableOnlyByTheGrantThatStagedIt(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	first, _ := r.grantFor(approval{
		label: "First", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend},
	})
	second, _ := r.grantFor(approval{
		label: "Second", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend},
	})

	minted := first.callOK("mail_upload_url", map[string]any{
		"filename": "contract.pdf", "mime_type": "application/pdf",
	})
	uploadURL := str(minted.payload["upload_url"])
	blobID := str(minted.payload["blob_id"])
	if uploadURL == "" || blobID == "" {
		t.Fatalf("mail_upload_url answered:\n%s", minted.text)
	}

	if status, body := r.put(uploadURL, []byte("%PDF-1.4 pretend")); status != http.StatusCreated {
		t.Fatalf("the upload answered %d: %s", status, body)
	}
	// Single use, and that is the whole of the check: a second PUT is a second writer.
	if status, _ := r.put(uploadURL, []byte("overwritten")); status != http.StatusConflict {
		t.Errorf("a second PUT to a single-use upload URL answered %d", status)
	}

	refused := second.callError("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "borrowed", "attachments": []map[string]any{{"blob_id": blobID}},
	})
	if !strings.Contains(refused.text, "no such blob") {
		t.Errorf("a second grant naming another grant's blob was refused with:\n%s", refused.text)
	}

	// The grant that staged it can attach it, and the bytes arrive.
	first.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "the contract", "attachments": []map[string]any{{"blob_id": blobID}},
	})
	sent := r.mailbox(work).sentMessages()
	if len(sent) != 1 || len(sent[0].Attachments) != 1 {
		t.Fatalf("the send carried %d messages / attachments: %+v", len(sent), sent)
	}
	if string(sent[0].Attachments[0].Content) != "%PDF-1.4 pretend" {
		t.Fatalf("the attachment carried %q", sent[0].Attachments[0].Content)
	}
}

// TestHeldSendKeepsItsAttachmentAfterTheBlobExpires is the interaction most likely to be
// broken, and is not.
//
// A blob lives fifteen minutes by default; the queue has no deadline at all. If the queue
// held a reference, an approval the next morning would send a message with its attachment
// silently missing, or fail outright. It holds the bytes instead — mail_send resolves every
// attachment before it decides to hold — so this drives the whole sequence with a TTL short
// enough to expire inside the test, sweeps the store the way the background sweeper does, and
// then approves.
func TestHeldSendKeepsItsAttachmentAfterTheBlobExpires(t *testing.T) {
	const ttl = 1500 * time.Millisecond
	r := newRig(t, options{attachmentTTL: ttl})
	work := r.link("work", "ada@work.example")

	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})

	minted := s.callOK("mail_upload_url", map[string]any{
		"filename": "contract.pdf", "mime_type": "application/pdf",
	})
	uploadURL := str(minted.payload["upload_url"])
	blobID := str(minted.payload["blob_id"])
	payload := []byte("%PDF-1.4 the signed contract")
	if status, body := r.put(uploadURL, payload); status != http.StatusCreated {
		t.Fatalf("the upload answered %d: %s", status, body)
	}

	res := s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "legal@example.net"}},
		"subject": "the contract", "body": "attached",
		"attachments": []map[string]any{{"blob_id": blobID}},
	})
	if res.payload["held"] != true {
		t.Fatalf("the send was not held:\n%s", res.text)
	}
	pending := r.pending()
	if len(pending) != 1 {
		t.Fatalf("the queue holds %d actions", len(pending))
	}

	// Wait past the TTL and run the sweeper, which is what the server does every five minutes.
	time.Sleep(ttl + 200*time.Millisecond)
	if _, err := r.blobs.Sweep(r.ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if _, _, err := r.blobs.Content(r.ctx, r.owner.ID, blobID); err == nil {
		t.Fatal("the blob outlived its TTL, so this test proves nothing")
	}

	resp := r.approveHeld(pending[0].ID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approving answered %d", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Location"), "failed=") {
		t.Fatalf("approving reported a failure: %s", resp.Header.Get("Location"))
	}

	sent := r.mailbox(work).sentMessages()
	if len(sent) != 1 {
		t.Fatalf("approving delivered %d messages", len(sent))
	}
	if len(sent[0].Attachments) != 1 {
		t.Fatalf("the approved send lost its attachment: %+v", sent[0])
	}
	if !bytes.Equal(sent[0].Attachments[0].Content, payload) {
		t.Fatalf("the approved send carried %q, not the staged bytes", sent[0].Attachments[0].Content)
	}
	if sent[0].Attachments[0].Filename != "contract.pdf" {
		t.Errorf("the attachment arrived as %q", sent[0].Attachments[0].Filename)
	}
}

// TestUploadURLNeedsAComposeCapability. Staged bytes can only ever become an attachment, so
// the permission that reaches them is the one that could attach them — not `attachments`,
// which is about reading mail out of a mailbox.
func TestUploadURLNeedsAComposeCapability(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	reader, _ := r.grantFor(approval{
		label: "Reader", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments},
	})
	for _, name := range reader.toolNames() {
		if name == "mail_upload_url" {
			t.Error("a grant that can only read mail was offered somewhere to write bytes")
		}
	}

	drafter, _ := r.grantFor(approval{
		label: "Drafter", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft},
	})
	minted := drafter.callOK("mail_upload_url", map[string]any{"filename": "note.txt"})
	if str(minted.payload["upload_url"]) == "" {
		t.Fatalf("a drafting grant could not mint an upload URL:\n%s", minted.text)
	}
}

// TestABlobLinkIsNotAnUploadLink. Both are genuine signatures over the same key; only the use
// claim keeps a read credential from being a write one.
func TestABlobLinkIsNotAnUploadLink(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	msg := r.mailbox(work).seed("quarterlies", "q3.txt", "text/plain", []byte("numbers"))

	s, _ := r.grantFor(approval{
		label: "Both", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead, mail.CapAttachments, mail.CapSend},
	})

	download := str(s.callOK("mail_get_attachment", map[string]any{
		"message_id": msg.String(), "attachment_id": "att1",
	}).payload["url"])
	minted := s.callOK("mail_upload_url", map[string]any{"filename": "note.txt"})
	upload := str(minted.payload["upload_url"])

	// A download token pasted onto the upload route, and the reverse.
	swapped := strings.Replace(download, "/attachments/", "/attachments/upload/", 1)
	if status, _ := r.put(swapped, []byte("written")); status == http.StatusCreated {
		t.Error("a download link was accepted as an upload URL")
	}
	uploadToken := strings.TrimPrefix(upload, r.baseURL+"/attachments/upload/")
	if status, body, _ := r.fetch(r.baseURL + "/attachments/" + uploadToken); status == http.StatusOK {
		t.Errorf("an upload URL was accepted as a download link: %s", body)
	}
}

// TestUploadURLReportsItsOwnExpiryAndTheBlobs is the regression test for a fix in this
// commit.
//
// The payload put `expires_at` beside `upload_url`, and the description beside it says "The
// URL works once and expires within minutes" — but the value was the blob's expiry, which is
// MAILROOM_ATTACHMENT_TTL and may be a whole day. The signature is capped at ten minutes by
// blob.uploadWindow whatever the TTL, so a client that believed the field held a URL it had
// long since lost. Both deadlines are now returned, each named for what it bounds.
func TestUploadURLReportsItsOwnExpiryAndTheBlobs(t *testing.T) {
	r := newRig(t, options{attachmentTTL: time.Hour})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Composer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend},
	})

	minted := s.callOK("mail_upload_url", map[string]any{"filename": "contract.pdf"})
	urlExpiry, err := time.Parse(time.RFC3339, str(minted.payload["expires_at"]))
	if err != nil {
		t.Fatalf("expires_at did not parse: %v\n%s", err, minted.text)
	}
	blobExpiry, err := time.Parse(time.RFC3339, str(minted.payload["blob_expires_at"]))
	if err != nil {
		t.Fatalf("blob_expires_at did not parse: %v\n%s", err, minted.text)
	}

	if life := time.Until(urlExpiry); life > 11*time.Minute {
		t.Errorf("expires_at is %s away; the signature is capped at ten minutes, so this is "+
			"the blob's expiry wearing the URL's name", life)
	}
	if life := time.Until(blobExpiry); life < 50*time.Minute {
		t.Errorf("blob_expires_at is %s away on an instance with an hour's TTL", life)
	}
	if !urlExpiry.Before(blobExpiry) {
		t.Errorf("the URL is reported as outliving the bytes it writes to: %s vs %s",
			urlExpiry, blobExpiry)
	}
}
