// Package blob_test exercises the routes end to end against a real HTTP server, a real
// database and real files on disk.
//
// External test package on purpose. internal/store implements the catalog, so it imports this
// package; testing from outside is what lets these tests use the real store rather than a
// stub that agrees with whatever the code does. It also means every check here goes through
// the exported surface, the same way a caller would.
package blob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/user"
	"github.com/tfyl/mailroom/internal/web"
)

type rig struct {
	t       *testing.T
	db      *store.Store
	blobs   *blob.Store
	signer  *blob.Signer
	server  *httptest.Server
	dir     string
	owner   user.ID
	other   user.ID
	grantID grant.ID
	account mail.Account
}

// newRig stands up the whole path: SQLite, a blob directory, the signer, the routes, and the
// same security-header middleware production runs — so a test that asserts on a response
// header is asserting on what a client would actually receive.
func newRig(t *testing.T, opts blob.Options) *rig {
	t.Helper()
	if opts.TTL == 0 {
		opts.TTL = 15 * time.Minute
	}

	root := t.TempDir()
	db, err := store.Open("sqlite://" + filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	dir := filepath.Join(root, "attachments")
	bytesStore, err := blob.NewDir(dir)
	if err != nil {
		t.Fatalf("creating the attachment directory: %v", err)
	}
	signer, err := blob.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("creating the signer: %v", err)
	}

	mux := http.NewServeMux()
	server := httptest.NewServer(web.SecurityHeaders(nil, mux))
	t.Cleanup(server.Close)

	blobs := blob.New(bytesStore, db, signer, server.URL, opts,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	blob.NewServer(blobs, db, db, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(mux)

	r := &rig{t: t, db: db, blobs: blobs, signer: signer, server: server, dir: dir}
	r.owner = r.signIn("alice")
	r.other = r.signIn("bob")
	r.account = r.link(r.owner, "acct_work", "work")
	r.grantID = r.newGrant(r.owner, []mail.AccountID{r.account.ID},
		mail.CapRead, mail.CapAttachments, mail.CapDraft, mail.CapSend)
	return r
}

func (r *rig) signIn(subject string) user.ID {
	r.t.Helper()
	u, _, err := r.db.EnsureUser(context.Background(), user.User{
		Issuer: "https://idp.example.com", Subject: subject, Email: subject + "@example.com",
	}, store.Admission{Policy: signup.Policy{Mode: signup.Open}})
	if err != nil {
		r.t.Fatalf("signing in %s: %v", subject, err)
	}
	return u.ID
}

func (r *rig) link(owner user.ID, id, alias string) mail.Account {
	r.t.Helper()
	a := mail.Account{
		ID: mail.AccountID(id), Alias: alias, Address: alias + "@example.com",
		Provider: mail.ProviderGmail, Status: mail.StatusLinked,
	}
	if err := r.db.LinkAccount(context.Background(), owner, a, "sealed", ""); err != nil {
		r.t.Fatalf("linking %s: %v", alias, err)
	}
	a.OwnerID = owner
	return a
}

func (r *rig) newGrant(owner user.ID, accounts []mail.AccountID, caps ...mail.Capability) grant.ID {
	r.t.Helper()
	ctx := context.Background()
	clientID := fmt.Sprintf("client_%s_%d", owner, time.Now().UnixNano())
	if err := r.db.RegisterClient(ctx, store.Client{
		ID: clientID, Name: "test client", RedirectURIs: []string{"https://client.example/cb"},
	}); err != nil {
		r.t.Fatalf("registering a client: %v", err)
	}
	g := &grant.Grant{
		ID:      grant.ID(fmt.Sprintf("grant_%s_%d", owner, time.Now().UnixNano())),
		OwnerID: owner, ClientID: clientID, Label: "test grant",
		Accounts: accounts, Caps: mail.NewSet(caps...),
	}
	if err := r.db.CreateGrant(ctx, g); err != nil {
		r.t.Fatalf("creating a grant: %v", err)
	}
	return g.ID
}

func (r *rig) putMail(content []byte, filename, mimeType string) blob.Link {
	r.t.Helper()
	link, err := r.blobs.PutMailAttachment(context.Background(), blob.MailPut{
		Owner: r.owner, GrantID: r.grantID, AccountID: r.account.ID,
		Filename: filename, MimeType: mimeType, Content: content,
	})
	if err != nil {
		r.t.Fatalf("storing an attachment: %v", err)
	}
	return link
}

func (r *rig) get(url string) *http.Response {
	r.t.Helper()
	res, err := r.server.Client().Get(url)
	if err != nil {
		r.t.Fatalf("GET %s: %v", url, err)
	}
	return res
}

// put sends a body whose length the transport does not know in advance, so the request goes
// out chunked. That is the shape the size limit actually has to survive: a client that
// declares no length, or a wrong one, must still be stopped by the writer.
func (r *rig) put(url string, body []byte) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, struct{ io.Reader }{bytes.NewReader(body)})
	if err != nil {
		r.t.Fatal(err)
	}
	res, err := r.server.Client().Do(req)
	if err != nil {
		r.t.Fatalf("PUT %s: %v", url, err)
	}
	return res
}

func (r *rig) tokenOf(url string) string {
	r.t.Helper()
	return url[strings.LastIndex(url, "/")+1:]
}

func (r *rig) onDisk(id string) bool {
	r.t.Helper()
	_, err := os.Stat(filepath.Join(r.dir, id))
	return err == nil
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(b)
}

// --- download ---

func TestSignedLinkServesTheFileWithItsHeaders(t *testing.T) {
	r := newRig(t, blob.Options{})
	content := []byte("%PDF-1.7\nthe quarterly invoice\n")
	link := r.putMail(content, "invoice Q3.pdf", "application/pdf")

	res := r.get(link.URL)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.StatusCode, body(t, res))
	}
	if got := body(t, res); got != string(content) {
		t.Errorf("the wrong bytes came back: %q", got)
	}

	for header, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Length":         fmt.Sprint(len(content)),
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, no-store, max-age=0",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s: want %q, got %q", header, want, got)
		}
	}
	// Always attachment, never inline, PDFs included: an inline render is third-party content
	// executing in the origin that holds the operator's session.
	if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") ||
		!strings.Contains(got, "invoice Q3.pdf") {
		t.Errorf("Content-Disposition should be an attachment naming the file, got %q", got)
	}
	// The response's own policy has to win over the app-wide one the middleware set, because
	// the app-wide one was written for documents this server rendered.
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("a blob response must carry its own sandboxing CSP, got %q", got)
	}
}

func TestTamperedSignatureIsRefused(t *testing.T) {
	r := newRig(t, blob.Options{})
	link := r.putMail([]byte("secret"), "notes.txt", "text/plain")

	// Flip a character of the MAC, leaving everything it covers intact.
	//
	// The second-to-last character, not the last. A 32-byte MAC is 43 base64 characters and
	// the final one carries two significant bits with four spare, so a great many distinct
	// last characters decode to the same signature — flipping it is a no-op about one time in
	// sixteen, and the fetch then legitimately succeeds. Every earlier character contributes
	// a full six bits, so mutating one always changes what the signature decodes to.
	const at = 2
	flipped := byte('A')
	if link.URL[len(link.URL)-at] == 'A' {
		flipped = 'B'
	}
	tampered := link.URL[:len(link.URL)-at] + string(flipped) + link.URL[len(link.URL)-at+1:]
	if tampered == link.URL {
		t.Fatal("the tampering changed nothing, so this proves nothing")
	}

	res := r.get(tampered)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a tampered signature must be refused, got %d: %s", res.StatusCode, body(t, res))
	}
	if got := body(t, res); strings.Contains(got, "notes.txt") || strings.Contains(got, "secret") {
		t.Errorf("a refusal must not echo the blob back: %q", got)
	}
}

func TestExpiredSignatureIsRefused(t *testing.T) {
	r := newRig(t, blob.Options{})
	link := r.putMail([]byte("secret"), "notes.txt", "text/plain")

	// A genuine signature over a moment that has passed. The blob itself is still there, so
	// this isolates the link's expiry from the bytes'.
	expired := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: link.ID, Owner: link.Owner,
		Grant: link.GrantID, Expires: time.Now().Add(-time.Second),
	})

	res := r.get(r.server.URL + "/attachments/" + expired)
	if res.StatusCode != http.StatusGone {
		t.Fatalf("an expired link must be refused, got %d: %s", res.StatusCode, body(t, res))
	}
}

// Splicing another blob's id into a link leaves the signature covering the old one, so the
// substitution is what fails rather than the lookup.
func TestASignatureForOneBlobDoesNotServeAnother(t *testing.T) {
	r := newRig(t, blob.Options{})
	mine := r.putMail([]byte("mine"), "mine.txt", "text/plain")
	theirs := r.putMail([]byte("theirs"), "theirs.txt", "text/plain")

	spliced := strings.Replace(r.tokenOf(mine.URL), mine.ID, theirs.ID, 1)
	res := r.get(r.server.URL + "/attachments/" + spliced)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.StatusCode, body(t, res))
	}
	if got := body(t, res); strings.Contains(got, "theirs") {
		t.Errorf("the refusal leaked the other blob: %q", got)
	}
}

// The strongest form of the ownership question: a signature that verifies, naming one owner
// and another owner's blob. The lookup is owner-scoped, so it resolves to nothing.
func TestAnotherOwnersBlobCannotBeFetched(t *testing.T) {
	r := newRig(t, blob.Options{})
	mine := r.putMail([]byte("alice's bank statement"), "statement.pdf", "application/pdf")

	bobAccount := r.link(r.other, "acct_bob", "bob-work")
	bobGrant := r.newGrant(r.other, []mail.AccountID{bobAccount.ID}, mail.CapAttachments)

	forged := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: mine.ID, Owner: r.other,
		Grant: bobGrant, Expires: time.Now().Add(time.Minute),
	})

	res := r.get(r.server.URL + "/attachments/" + forged)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("bob must not reach alice's blob, got %d: %s", res.StatusCode, body(t, res))
	}
	if got := body(t, res); strings.Contains(got, "statement") {
		t.Errorf("the refusal leaked the filename: %q", got)
	}
}

// The other half: the right owner, the wrong grant. The blob names the grant that minted it,
// so a second grant of the same user cannot pick up its links.
func TestABlobIsNotReachableThroughADifferentGrant(t *testing.T) {
	r := newRig(t, blob.Options{})
	link := r.putMail([]byte("payload"), "file.txt", "text/plain")
	second := r.newGrant(r.owner, []mail.AccountID{r.account.ID}, mail.CapAttachments)

	token := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: link.ID, Owner: r.owner,
		Grant: second, Expires: time.Now().Add(time.Minute),
	})

	res := r.get(r.server.URL + "/attachments/" + token)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", res.StatusCode, body(t, res))
	}
}

// The decision this feature turns on: a signed URL does not outlive the grant that minted it.
func TestRevokingAGrantKillsItsOutstandingLinks(t *testing.T) {
	r := newRig(t, blob.Options{})
	link := r.putMail([]byte("still here"), "file.txt", "text/plain")

	if res := r.get(link.URL); res.StatusCode != http.StatusOK {
		t.Fatalf("the link should work before revocation, got %d", res.StatusCode)
	}
	if err := r.db.RevokeGrant(context.Background(), r.owner, r.grantID); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	res := r.get(link.URL)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a revoked grant must kill its links, got %d: %s", res.StatusCode, body(t, res))
	}
}

// Narrowing is revoking part of a grant, and the consent screen presents it as the same
// decision, so an outstanding link has to answer to it too.
func TestNarrowingAGrantKillsItsOutstandingLinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []mail.AccountID
		caps     mail.Set
	}{
		{"the capability is dropped", nil, mail.NewSet(mail.CapRead)},
		{"the mailbox is dropped", []mail.AccountID{}, mail.NewSet(mail.CapAttachments)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, blob.Options{})
			link := r.putMail([]byte("still here"), "file.txt", "text/plain")

			accounts := tc.accounts
			if accounts == nil {
				accounts = []mail.AccountID{r.account.ID}
			}
			if err := r.db.EditGrant(context.Background(), r.owner, r.grantID, accounts, tc.caps, grant.DefaultMode, nil); err != nil {
				t.Fatalf("editing the grant: %v", err)
			}

			res := r.get(link.URL)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", res.StatusCode, body(t, res))
			}
		})
	}
}

// Removing a revoked grant is not a way to leave its attachments behind.
//
// The link dies with the grant, as revoking already made it — every fetch re-reads the grant,
// and a removed one is not there to read. What removal adds is that the row and the bytes go
// too, rather than sitting on the volume until their own expiry: the removal expires them,
// and the sweeper deletes the file before the row that finds it.
func TestRemovingARevokedGrantTakesItsAttachmentsWithIt(t *testing.T) {
	r := newRig(t, blob.Options{TTL: time.Hour})
	ctx := context.Background()
	// Two, because there are two ways a blob leaves: the read path drops one it finds past
	// its expiry, and the sweeper takes the ones nobody asks for again — which, after a
	// removal, is all of them.
	fetched := r.putMail([]byte("last quarter's numbers"), "numbers.pdf", "application/pdf")
	untouched := r.putMail([]byte("the other one"), "notes.txt", "text/plain")

	if res := r.get(fetched.URL); res.StatusCode != http.StatusOK {
		t.Fatalf("the link should work before any of this, got %d", res.StatusCode)
	}
	if err := r.db.RevokeGrant(ctx, r.owner, r.grantID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if err := r.db.RemoveGrant(ctx, r.owner, r.grantID); err != nil {
		t.Fatalf("removing: %v", err)
	}

	// The signature is still valid and still an hour from its own expiry, so this is the
	// server refusing rather than the token running out.
	if res := r.get(fetched.URL); res.StatusCode == http.StatusOK {
		t.Fatalf("a removed grant must not serve its attachments, got %d: %s",
			res.StatusCode, body(t, res))
	}
	if r.onDisk(fetched.ID) {
		t.Error("the read path should have dropped the bytes on its way past")
	}

	n, err := r.blobs.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Fatalf("want the blob nobody fetched swept, got %d", n)
	}
	for _, id := range []string{fetched.ID, untouched.ID} {
		if r.onDisk(id) {
			t.Errorf("%s is still on disk", id)
		}
		if _, err := r.db.Blob(ctx, r.owner, id); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("%s still has a row, got %v", id, err)
		}
	}
}

func TestExpiryDeletesTheBytesFromDisk(t *testing.T) {
	r := newRig(t, blob.Options{TTL: 40 * time.Millisecond})
	link := r.putMail([]byte("temporary"), "file.txt", "text/plain")

	if !r.onDisk(link.ID) {
		t.Fatal("the bytes should be on disk immediately after storing")
	}
	time.Sleep(60 * time.Millisecond)

	n, err := r.blobs.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Fatalf("want one blob swept, got %d", n)
	}
	if r.onDisk(link.ID) {
		t.Error("expiry must delete the file, not just the row")
	}
	if _, err := r.db.Blob(context.Background(), r.owner, link.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the row should be gone too, got %v", err)
	}
	if res := r.get(link.URL); res.StatusCode != http.StatusGone {
		t.Errorf("want 410 after expiry, got %d", res.StatusCode)
	}
}

// Expiry is enforced on every read as well as by the sweeper, so a stalled sweeper cannot
// leave a blob being handed out past its time. The download route never reaches this — its
// token expires at the same instant — but the send path reads a blob by id, and an upload
// deliberately outlives the URL that created it.
func TestAnExpiredBlobIsGoneOnReadWithoutASweep(t *testing.T) {
	r := newRig(t, blob.Options{TTL: 40 * time.Millisecond})
	link := r.putMail([]byte("temporary"), "file.txt", "text/plain")
	time.Sleep(60 * time.Millisecond)

	if _, _, err := r.blobs.Content(context.Background(), r.owner, link.ID); !errors.Is(err, blob.ErrGone) {
		t.Fatalf("want ErrGone, got %v", err)
	}
	if r.onDisk(link.ID) {
		t.Error("the read path should have dropped the expired bytes on its way past")
	}
}

// A blob whose bytes will not delete keeps its row, and the next sweep tries again.
//
// The row is the only thing that knows the file exists: nothing in this package walks the
// attachment directory, so dropping the row on a failed unlink would leave the bytes on the
// volume with nothing pointing at them and nothing charged for them — a leak that no later
// pass could find. This is the narrow case it guards, a volume that refuses rather than a
// file that has already gone, which Dir.Delete reports as success.
func TestBytesThatWillNotDeleteKeepTheirRowForTheNextSweep(t *testing.T) {
	r := newRig(t, blob.Options{TTL: 40 * time.Millisecond})
	ctx := context.Background()

	dir, err := blob.NewDir(r.dir)
	if err != nil {
		t.Fatalf("reopening the attachment directory: %v", err)
	}
	refusing := &refusingDelete{Bytes: dir, err: errors.New("read-only file system")}
	blobs := blob.New(refusing, r.db, r.signer, r.server.URL,
		blob.Options{TTL: 40 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	link, err := blobs.PutMailAttachment(ctx, blob.MailPut{
		Owner: r.owner, GrantID: r.grantID, AccountID: r.account.ID,
		Filename: "file.txt", MimeType: "text/plain", Content: []byte("temporary"),
	})
	if err != nil {
		t.Fatalf("storing an attachment: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	n, err := blobs.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 0 {
		t.Errorf("a blob whose bytes stayed on disk was counted as swept: %d", n)
	}
	if !r.onDisk(link.ID) {
		t.Fatal("the file went after all, so this is not exercising a failed unlink")
	}
	if _, err := r.db.Blob(ctx, r.owner, link.ID); err != nil {
		t.Fatalf("the row has to survive for the next sweep to find the file again, got %v", err)
	}
	// Surviving is not the same as being reachable. The row is past its expiry either way.
	if _, _, err := blobs.Content(ctx, r.owner, link.ID); !errors.Is(err, blob.ErrGone) {
		t.Errorf("an expired blob must stay gone whether or not its bytes deleted, got %v", err)
	}

	// The volume recovers, and the retry the surviving row made possible finishes the job.
	refusing.err = nil
	n, err = blobs.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweeping after the volume recovered: %v", err)
	}
	if n != 1 {
		t.Fatalf("want the retry to sweep the blob, got %d", n)
	}
	if r.onDisk(link.ID) {
		t.Error("the retry should have removed the file")
	}
	if _, err := r.db.Blob(ctx, r.owner, link.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the row should be gone once its bytes are, got %v", err)
	}
}

// refusingDelete is a Bytes whose unlink fails until err is cleared, which is the only way to
// reach the failure from outside: Dir.Delete on a real directory succeeds, and succeeds again
// on a key that is not there.
type refusingDelete struct {
	blob.Bytes
	err error
}

func (b *refusingDelete) Delete(ctx context.Context, key string) error {
	if b.err != nil {
		return b.err
	}
	return b.Bytes.Delete(ctx, key)
}

// --- upload ---

func (r *rig) newUpload(filename, mimeType string, max int64) blob.Upload {
	r.t.Helper()
	up, err := r.blobs.NewUpload(context.Background(), blob.UploadRequest{
		Owner: r.owner, GrantID: r.grantID, Filename: filename, MimeType: mimeType, MaxBytes: max,
	})
	if err != nil {
		r.t.Fatalf("minting an upload URL: %v", err)
	}
	return up
}

func TestUploadRoundTrips(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("report.pdf", "application/pdf", 0)
	if up.Method != http.MethodPut {
		t.Fatalf("the client is told to use %q", up.Method)
	}

	content := []byte("%PDF-1.7\nthe report\n")
	res := r.put(up.URL, content)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", res.StatusCode, body(t, res))
	}
	if got := body(t, res); !strings.Contains(got, up.ID) {
		t.Errorf("the response should name the blob to reference, got %s", got)
	}

	ref, stored, err := r.blobs.Content(context.Background(), r.owner, up.ID)
	if err != nil {
		t.Fatalf("reading back the upload: %v", err)
	}
	if !bytes.Equal(stored, content) {
		t.Errorf("the bytes that arrived are not the bytes that were sent: %q", stored)
	}
	if ref.Size != int64(len(content)) {
		t.Errorf("size recorded as %d, want %d", ref.Size, len(content))
	}
}

// The limit is on the writer, not on Content-Length, so a body that declares nothing is still
// stopped — and a partial file must not survive the refusal.
func TestAnOversizedUploadIsRefusedAndLeavesNothing(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("big.bin", "application/octet-stream", 1024)

	res := r.put(up.URL, bytes.Repeat([]byte("x"), 4096))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d: %s", res.StatusCode, body(t, res))
	}
	if r.onDisk(up.ID) {
		t.Error("a refused upload must leave no file behind")
	}
	if _, err := r.db.Blob(context.Background(), r.owner, up.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("a refused upload must leave no row behind, got %v", err)
	}
}

// One URL, one body. Otherwise the bytes behind a blob id somebody already holds could be
// swapped after the fact.
func TestAnUploadURLWorksOnce(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	if res := r.put(up.URL, []byte("first")); res.StatusCode != http.StatusCreated {
		t.Fatalf("the first PUT should succeed, got %d: %s", res.StatusCode, body(t, res))
	}
	res := r.put(up.URL, []byte("second"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 on reuse, got %d: %s", res.StatusCode, body(t, res))
	}

	_, stored, err := r.blobs.Content(context.Background(), r.owner, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "first" {
		t.Errorf("the second PUT rewrote the blob: %q", stored)
	}
}

func TestATamperedUploadSignatureIsRefused(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	// The second-to-last character, for the reason spelled out in
	// TestTamperedSignatureIsRefused: the last one has spare bits, so flipping it changes
	// nothing about one time in sixteen and the upload legitimately succeeds.
	const at = 2
	flipped := byte('A')
	if up.URL[len(up.URL)-at] == 'A' {
		flipped = 'B'
	}
	tampered := up.URL[:len(up.URL)-at] + string(flipped) + up.URL[len(up.URL)-at+1:]
	if tampered == up.URL {
		t.Fatal("the tampering changed nothing, so this proves nothing")
	}
	if res := r.put(tampered, []byte("hello")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.StatusCode, body(t, res))
	}
	if r.onDisk(up.ID) {
		t.Error("a refused upload must not have written anything")
	}
}

func TestAnExpiredUploadSignatureIsRefused(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	expired := r.signer.Token(blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: up.Owner, Grant: up.GrantID,
		Expires: time.Now().Add(-time.Second), Max: up.MaxBytes,
	})
	res := r.put(r.server.URL+"/attachments/upload/"+expired, []byte("hello"))
	if res.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d: %s", res.StatusCode, body(t, res))
	}
}

// A genuine signature for the wrong route is still refused: reading is not permission to
// write, and writing is not permission to read.
func TestALinkIsNotValidOnTheOtherRoute(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)
	if res := r.put(up.URL, []byte("hello")); res.StatusCode != http.StatusCreated {
		t.Fatal("setting up: the upload should succeed")
	}

	asDownload := r.signer.Token(blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: up.Owner, Grant: up.GrantID,
		Expires: time.Now().Add(time.Minute), Max: up.MaxBytes,
	})
	if res := r.get(r.server.URL + "/attachments/" + asDownload); res.StatusCode != http.StatusForbidden {
		t.Errorf("an upload token must not read, got %d", res.StatusCode)
	}

	link := r.putMail([]byte("mail"), "mail.txt", "text/plain")
	asUpload := r.signer.Token(blob.Claims{
		Use: blob.UseDownload, BlobID: link.ID, Owner: link.Owner, Grant: link.GrantID,
		Expires: time.Now().Add(time.Minute),
	})
	if res := r.put(r.server.URL+"/attachments/upload/"+asUpload, []byte("x")); res.StatusCode != http.StatusForbidden {
		t.Errorf("a download token must not write, got %d", res.StatusCode)
	}
}

// An upload URL is a write primitive, so the owner it was signed for is the only owner it can
// write for. Naming somebody else's blob resolves to nothing, and the blob it aimed at is
// untouched.
func TestAnUploadURLCannotWriteAnotherOwnersBlob(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	bobAccount := r.link(r.other, "acct_bob", "bob-work")
	bobGrant := r.newGrant(r.other, []mail.AccountID{bobAccount.ID}, mail.CapSend)

	forged := r.signer.Token(blob.Claims{
		Use: blob.UseUpload, BlobID: up.ID, Owner: r.other, Grant: bobGrant,
		Expires: time.Now().Add(time.Minute), Max: blob.MaxUpload,
	})
	if res := r.put(r.server.URL+"/attachments/upload/"+forged, []byte("bob's bytes")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.StatusCode, body(t, res))
	}

	ref, err := r.db.Blob(context.Background(), r.owner, up.ID)
	if err != nil {
		t.Fatalf("alice's blob should still exist: %v", err)
	}
	if ref.State != blob.StatePending {
		t.Errorf("alice's upload should still be waiting for her bytes, got state %q", ref.State)
	}
}

// The highest-risk part of the feature: bytes a client chose, served back from the origin
// that holds the operator's session. An uploaded page must arrive as an inert download.
func TestAnUploadedPageCannotExecuteWhenFetchedBack(t *testing.T) {
	r := newRig(t, blob.Options{})
	for _, declared := range []string{"text/html", "image/svg+xml", "application/xhtml+xml"} {
		t.Run(declared, func(t *testing.T) {
			up := r.newUpload("payload.html", declared, 0)
			if res := r.put(up.URL, []byte("<script>fetch('/grants')</script>")); res.StatusCode != http.StatusCreated {
				t.Fatalf("uploading: %d", res.StatusCode)
			}

			ref, err := r.db.Blob(context.Background(), r.owner, up.ID)
			if err != nil {
				t.Fatal(err)
			}
			res := r.get(r.blobs.DownloadURL(ref))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", res.StatusCode, body(t, res))
			}
			if got := res.Header.Get("Content-Type"); got != "application/octet-stream" {
				t.Errorf("a caller's content type must never be reflected back, got %q", got)
			}
			if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
				t.Errorf("want an attachment disposition, got %q", got)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("want nosniff, got %q", got)
			}
			if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") ||
				!strings.Contains(got, "default-src 'none'") {
				t.Errorf("want a sandboxing CSP on the response itself, got %q", got)
			}
		})
	}
}

func TestServableTypeKeepsInertTypesAndRewritesTheRest(t *testing.T) {
	for declared, want := range map[string]string{
		"application/pdf":           "application/pdf",
		"text/plain; charset=utf-8": "text/plain",
		"image/png":                 "image/png",
		"text/html":                 "application/octet-stream",
		"image/svg+xml":             "application/octet-stream",
		"application/xml":           "application/octet-stream",
		"application/javascript":    "application/octet-stream",
		"":                          "application/octet-stream",
		"not a media type":          "application/octet-stream",
	} {
		if got := blob.ServableType(declared); got != want {
			t.Errorf("%q: want %q, got %q", declared, want, got)
		}
	}
}

// Uploads let a grant holder spend the mail server's disk, so there has to be a point at
// which it says no rather than filling the volume the database is on.
func TestAPerOwnerQuotaRefusesFurtherUploads(t *testing.T) {
	r := newRig(t, blob.Options{OwnerQuota: 4096, InstanceCap: 1 << 20})

	// A pending upload is charged the size it was promised, not the nothing it holds — so
	// minting a second one that would take the owner over is refused before any bytes exist.
	if _, err := r.blobs.NewUpload(context.Background(), blob.UploadRequest{
		Owner: r.owner, GrantID: r.grantID, Filename: "a.bin", MaxBytes: 3000,
	}); err != nil {
		t.Fatalf("the first upload should fit: %v", err)
	}
	_, err := r.blobs.NewUpload(context.Background(), blob.UploadRequest{
		Owner: r.owner, GrantID: r.grantID, Filename: "b.bin", MaxBytes: 3000,
	})
	if !errors.Is(err, blob.ErrQuota) {
		t.Fatalf("want a quota refusal, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "allowance") {
		t.Errorf("the refusal should say what the limit is: %v", err)
	}
}

func TestAnUploadIsNotReadableUntilItHasArrived(t *testing.T) {
	r := newRig(t, blob.Options{})
	up := r.newUpload("notes.txt", "text/plain", 0)

	if _, _, err := r.blobs.Content(context.Background(), r.owner, up.ID); !errors.Is(err, blob.ErrNotReady) {
		t.Fatalf("a pending upload has no content, got %v", err)
	}
}
