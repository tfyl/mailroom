package blob_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"

	"github.com/tfyl/mailroom/internal/blob"
)

// What these routes say when they refuse.
//
// Everything else in this package tests the status code, which is what stops the bytes. These
// test the sentence, which is what decides whether the client tries the right thing next. An
// agent holding a dead link has exactly two useful questions — is the file gone, and what call
// gets me a working one — and a refusal that answers neither sends it back to the mailbox
// looking for a message that was never the problem.
//
// Each case asserts the status too, so a message rewritten into the wrong branch fails here
// rather than passing on a substring.

func mustSay(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q:\n%s", what, want, text)
		}
	}
}

// An expired upload URL. The only correct next move is another mail_upload_url, and the
// message has to name it — "this link has expired; ask for a new one" tells an agent with no
// human in front of it nothing it can act on.
func TestAnExpiredUploadURLSaysToMintAnother(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	expired := r.signer.Token(blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: up.Owner, Grant: up.GrantID,
		Expires: time.Now().Add(-time.Second), Max: up.MaxBytes,
	})
	res := r.put(r.server.URL+"/attachments/upload/"+expired, []byte("hello"))
	if res.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d", res.StatusCode)
	}
	mustSay(t, "an expired upload URL", body(t, res),
		"mail_upload_url", "nothing was written")
}

// A used upload URL. Same answer, different reason, and it has to be clear that the first PUT
// stands: a client told only "already used" may believe its bytes are in place when they are
// the wrong ones.
func TestAUsedUploadURLSaysToMintAnother(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)
	if res := r.put(up.URL, []byte("first")); res.StatusCode != http.StatusCreated {
		t.Fatalf("setting up: the first PUT answered %d", res.StatusCode)
	}

	res := r.put(up.URL, []byte("second"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", res.StatusCode)
	}
	mustSay(t, "a reused upload URL", body(t, res),
		"exactly one", "mail_upload_url")
}

// A body over the ceiling. The refusal happens part-way through the write, so the one thing a
// client must not do is send the same file again — and it cannot know what would fit unless
// the number is in the message.
func TestAnOversizedUploadNamesTheCeilingAndSaysNotToRetry(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("big.bin", "application/octet-stream", 4<<20)

	res := r.put(up.URL, bytes.Repeat([]byte("x"), (4<<20)+1024))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", res.StatusCode)
	}
	mustSay(t, "an oversized upload", body(t, res),
		"4 MiB", "same file again", "smaller")
}

// A download link fetched after the grant behind it was narrowed.
//
// The blob is still on disk and the message is still in the mailbox; what changed is the
// permission. Saying "not found" would send a client back to search for a message that is
// exactly where it was, so this says the access changed and who can undo it.
func TestANarrowedGrantsLinkSaysTheAccessChanged(t *testing.T) {
	r := newRig(t, blob.Options{})
	link := r.putMail([]byte("still here"), "file.txt", "text/plain")

	if err := r.db.EditGrant(context.Background(), r.owner, r.grantID,
		[]mail.AccountID{r.account.ID}, mail.NewSet(mail.CapRead), grant.DefaultMode, nil); err != nil {
		t.Fatalf("narrowing the grant: %v", err)
	}

	res := r.get(link.URL)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", res.StatusCode)
	}
	mustSay(t, "a link refused after its grant was narrowed", body(t, res),
		"no longer authorized", "has not gone anywhere", "mail_get_attachment")
}

// An expired download link. The copy on this server is what expired; the attachment itself is
// untouched, and the fix is one call rather than a re-read of the whole message.
func TestAnExpiredDownloadLinkSaysTheMailIsStillThere(t *testing.T) {
	r := newRig(t, blob.Options{TTL: 40 * time.Millisecond})
	link := r.putMail([]byte("temporary"), "file.txt", "text/plain")
	time.Sleep(60 * time.Millisecond)

	res := r.get(link.URL)
	if res.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d", res.StatusCode)
	}
	mustSay(t, "an expired download link", body(t, res),
		"still in the mailbox", "mail_get_attachment")
}

// A blob_id whose bytes never arrived, fetched back through its own download URL. "Not found"
// is true and points at the wrong thing: the reservation exists and the PUT is what is
// missing.
func TestAnUnwrittenUploadSaysThePutNeverHappened(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	token := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: up.ID, Owner: up.Owner, Grant: up.GrantID,
		Expires: time.Now().Add(time.Minute),
	})
	res := r.get(r.server.URL + "/attachments/" + token)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", res.StatusCode)
	}
	mustSay(t, "an unwritten upload", body(t, res),
		"no PUT ever completed", "mail_upload_url")
}

// A token that names nothing. The message must not invite a retry — the same link will fail
// the same way for ever — and must name the call that produces a live one for whichever route
// it arrived on.
func TestAnUnknownLinkNamesTheRightToolPerRoute(t *testing.T) {
	r := newRig(t, blob.Options{})

	download := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: "blob_nothing", Owner: r.owner, Grant: r.grantID,
		Expires: time.Now().Add(time.Minute),
	})
	res := r.get(r.server.URL + "/attachments/" + download)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 on the download route, got %d", res.StatusCode)
	}
	mustSay(t, "an unknown download link", body(t, res),
		"Do not retry", "mail_get_attachment")

	upload := r.signer.Token(blob.Claims{
		Use: blob.UseUpload, BlobID: "blob_nothing", Owner: r.owner, Grant: r.grantID,
		Expires: time.Now().Add(time.Minute), Max: 1024,
	})
	res = r.put(r.server.URL+"/attachments/upload/"+upload, []byte("x"))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 on the upload route, got %d", res.StatusCode)
	}
	mustSay(t, "an unknown upload URL", body(t, res),
		"Do not retry", "mail_upload_url")
}
