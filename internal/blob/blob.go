// Package blob keeps attachment bytes out of the MCP conversation.
//
// The problem it exists for is one-directional at first glance and is not. Downloading, an
// attachment returned inside a tool result is base64 sitting in the model's context — a 5 MB
// PDF costs about 6.7 MB of context and buys nothing, because a model cannot read a PDF out
// of its own transcript anyway. Uploading, the same encoding runs into the transport's 4 MiB
// request ceiling, so anything a person would actually want to send does not fit at all.
//
// Both directions are solved the same way: the bytes live on the deployment's own data
// volume, and what crosses the conversation is a short-lived signed URL the client fetches or
// writes to itself. MCP gives this server no access to the client's filesystem, so a client
// performing its own HTTP request is the only way an agent's local file can ever reach here.
//
// A signed URL is a credential, so every property of one is deliberately narrow. It expires,
// it is signed with a key derived for nothing else, it names the grant that minted it, and
// every request re-reads that grant — so revoking a grant kills its links immediately rather
// than at their own expiry. The bytes are a short-lived copy of something the grant could
// already reach; they are not a second, longer-lived way to reach it.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Kind separates a copy of something already in a mailbox from bytes a client supplied.
//
// It decides what a blob may be used for and which capability reaches it, so it is stored
// rather than inferred: a mail copy is a re-reading of mail and answers to `attachments`,
// while an upload is something the client already had and answers to whichever capability
// could attach it to a message.
type Kind string

const (
	KindMail   Kind = "mail"
	KindUpload Kind = "upload"
)

// State tracks an upload through its one permitted write.
//
// A minted upload URL creates the row as pending. The PUT that claims it moves it to
// uploading, and only a completed body makes it ready. Nothing reads a row that is not
// ready, which is what stops a half-written body being attached to a message.
type State string

const (
	StatePending   State = "pending"
	StateUploading State = "uploading"
	StateReady     State = "ready"
)

var (
	ErrNotFound = errors.New("no such blob")
	// ErrGone separates "this expired" from "this never existed", because the two want
	// different things from a caller: mint another link, versus stop asking.
	ErrGone     = errors.New("blob has expired")
	ErrClaimed  = errors.New("this upload URL has already been used")
	ErrNotReady = errors.New("nothing has been uploaded to this blob yet")
	ErrTooLarge = errors.New("body is larger than this upload allows")
	ErrQuota    = errors.New("attachment storage is full")
)

// MaxUpload is the largest body an upload URL will accept.
//
// It is mail.MaxAttachmentBytes rather than a number of its own so the two paths cannot
// disagree about what a mail server will take. An upload larger than a whole message could
// hold would be accepted, stored, and then refused at send — having already spent the disk.
const MaxUpload = mail.MaxAttachmentBytes

// Link lifetimes are constants rather than configuration.
//
// The blob's own retention is the setting an operator has a reason to change; how long a URL
// stays usable inside that window is a security property, and one worth being the same
// everywhere. A download link runs to the blob's expiry because the blob exists for that one
// fetch. An upload URL is shorter still: it is needed for the moments between a tool call and
// the client's PUT, and it is a write primitive, so it should not outlive that.
const uploadWindow = 10 * time.Minute

// Ref is a blob's metadata. The bytes live in a Bytes implementation; everything that decides
// who may reach them lives here, in the database, where it can be re-read on every request.
type Ref struct {
	ID      string
	Owner   user.ID
	GrantID grant.ID
	Kind    Kind
	State   State
	// AccountID is the mailbox a mail copy came from, so a fetch can check the grant still
	// covers that mailbox. Empty for an upload, which came from no mailbox.
	AccountID mail.AccountID
	Filename  string
	MimeType  string
	Size      int64
	// Reserved is the disk this row is charged for while it holds no bytes. A pending upload
	// is charged its maximum: otherwise a caller could mint a hundred upload URLs, each
	// promising 18 MiB, and every quota check would see an empty store.
	Reserved  int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Charged is the disk this row is accounted for, whether or not the bytes have arrived.
func (r Ref) Charged() int64 {
	if r.State == StateReady {
		return r.Size
	}
	return r.Reserved
}

// Requires reports the capabilities that reach this blob, any one of which is enough.
//
// A mail copy is mail, and answers to the capability that was needed to read it in the first
// place. An upload is the client's own bytes, and the only thing it can be used for is being
// attached to a message — so whichever capability could attach it is the one that reaches it,
// and a grant that can no longer compose can no longer read back what it staged.
func (r Ref) Requires() []mail.Capability {
	if r.Kind == KindUpload {
		return []mail.Capability{mail.CapDraft, mail.CapSend}
	}
	return []mail.Capability{mail.CapAttachments}
}

// Bytes is where the content actually sits. The interface is three methods wide on purpose:
// it is the seam an S3-compatible backend slots into without anything else in this package
// learning that object storage exists.
type Bytes interface {
	Put(ctx context.Context, key string, r io.Reader, limit int64) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, key string) error
}

// Catalog persists blob metadata. Implemented by internal/store against the same database as
// everything else, so a blob's owner and grant are queried the same way a mailbox's are.
type Catalog interface {
	PutBlob(ctx context.Context, owner user.ID, r Ref) error
	Blob(ctx context.Context, owner user.ID, id string) (Ref, error)
	DeleteBlob(ctx context.Context, owner user.ID, id string) error
	// ClaimBlob moves a pending upload to uploading and returns the row it moved, or
	// ErrClaimed if it was not pending. It is the single-use check, and it has to be one
	// statement: two concurrent PUTs that both read `pending` and then both write would each
	// believe they had the URL.
	ClaimBlob(ctx context.Context, owner user.ID, id string, now time.Time) (Ref, error)
	CompleteBlob(ctx context.Context, owner user.ID, id string, size int64) error
	// ExpiredBlobs is the one lookup with no owner, because sweeping is not somebody's read
	// of their own data — it is maintenance across every owner at once. The rows it returns
	// carry their owner, so the deletes that follow are scoped like everything else.
	ExpiredBlobs(ctx context.Context, before time.Time) ([]Ref, error)
	OwnerBlobBytes(ctx context.Context, owner user.ID) (int64, error)
	TotalBlobBytes(ctx context.Context) (int64, error)
}

// Options are the operator-settable parts. Everything else about a blob's lifetime is fixed.
type Options struct {
	// TTL is how long bytes stay on disk. It is also exactly how long a download link lasts,
	// because a mail copy exists for one fetch and there is nothing to keep afterwards.
	TTL time.Duration
	// OwnerQuota caps one user's share, so a single grant cannot fill the volume the mail
	// database is on. InstanceCap caps the lot.
	OwnerQuota  int64
	InstanceCap int64
}

type Store struct {
	bytes   Bytes
	catalog Catalog
	signer  *Signer
	baseURL string
	opts    Options
	now     func() time.Time
	log     *slog.Logger
}

// New assembles the store. The logger is required rather than defaulted because the only
// things this package logs are failures to delete bytes, and those are invisible everywhere
// else: nothing enumerates the attachment directory, so a file that will not go is a file
// nothing will ever mention again.
func New(b Bytes, c Catalog, signer *Signer, baseURL string, opts Options, log *slog.Logger) *Store {
	return &Store{
		bytes: b, catalog: c, signer: signer,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		opts:    opts, now: time.Now, log: log,
	}
}

// Link is what a tool hands back instead of the bytes.
type Link struct {
	Ref
	URL string
}

// Upload is what the upload tool hands back: everything a client needs to perform the PUT
// itself, with nothing left to guess.
//
// URLExpires is separate from Ref.ExpiresAt and both are needed, because they are two
// different deadlines and a client has to respect each. Ref.ExpiresAt is when the bytes go,
// which bounds how long the blob_id is worth naming in a message. URLExpires is when the
// signature stops being accepted, which is sooner — uploadWindow caps it at ten minutes
// however long the operator lets blobs live. Reporting the blob's expiry as the URL's told a
// client on a day-long TTL that it had a day to perform a PUT it had ten minutes for.
type Upload struct {
	Ref
	URL        string
	Method     string
	MaxBytes   int64
	URLExpires time.Time
}

// MailPut is a copy of an attachment that is already in a mailbox.
type MailPut struct {
	Owner     user.ID
	GrantID   grant.ID
	AccountID mail.AccountID
	Filename  string
	MimeType  string
	Content   []byte
}

// PutMailAttachment stores an attachment's bytes and returns a link to fetch them.
func (s *Store) PutMailAttachment(ctx context.Context, req MailPut) (Link, error) {
	now := s.now()
	size := int64(len(req.Content))
	if err := s.reserve(ctx, req.Owner, size); err != nil {
		return Link{}, err
	}

	ref := Ref{
		ID:        ids.New("blob"),
		Owner:     req.Owner,
		GrantID:   req.GrantID,
		Kind:      KindMail,
		State:     StateReady,
		AccountID: req.AccountID,
		Filename:  SafeFilename(req.Filename),
		MimeType:  normalizeType(req.MimeType),
		Size:      size,
		CreatedAt: now,
		ExpiresAt: now.Add(s.opts.TTL),
	}

	// Bytes first, row second. The other order leaves a row promising content that is not
	// there, which reads to every caller as a server fault; this order can only leave an
	// orphan file, and the rollback below is what removes it. Nothing walks the attachment
	// directory looking for files no row names — every deletion in this package is driven
	// from a row — so if that rollback fails the bytes are unreachable rather than merely
	// stale, and the log line is the only trace of them there will ever be.
	if _, err := s.bytes.Put(ctx, ref.ID, bytes.NewReader(req.Content), size); err != nil {
		return Link{}, err
	}
	if err := s.catalog.PutBlob(ctx, req.Owner, ref); err != nil {
		if rmErr := s.bytes.Delete(ctx, ref.ID); rmErr != nil {
			s.log.Error("an attachment's row failed to write and its bytes would not roll back; "+
				"they are orphaned on disk and nothing will find them again",
				"blob", ref.ID, "err", rmErr)
		}
		return Link{}, err
	}
	return Link{Ref: ref, URL: s.downloadURL(ref)}, nil
}

// UploadRequest asks for somewhere to put a file the client already holds.
type UploadRequest struct {
	Owner    user.ID
	GrantID  grant.ID
	Filename string
	MimeType string
	// MaxBytes is what the caller expects to send. It only ever narrows: a caller cannot ask
	// for more than MaxUpload, and the number that ends up in the signature is the ceiling
	// the route enforces while writing.
	MaxBytes int64
}

// NewUpload mints a single-use upload URL.
func (s *Store) NewUpload(ctx context.Context, req UploadRequest) (Upload, error) {
	limit := req.MaxBytes
	if limit <= 0 || limit > MaxUpload {
		limit = MaxUpload
	}
	if err := s.reserve(ctx, req.Owner, limit); err != nil {
		return Upload{}, err
	}

	now := s.now()
	expires := now.Add(s.opts.TTL)
	ref := Ref{
		ID:        ids.New("blob"),
		Owner:     req.Owner,
		GrantID:   req.GrantID,
		Kind:      KindUpload,
		State:     StatePending,
		Filename:  SafeFilename(req.Filename),
		MimeType:  normalizeType(req.MimeType),
		Reserved:  limit,
		CreatedAt: now,
		ExpiresAt: expires,
	}
	if err := s.catalog.PutBlob(ctx, req.Owner, ref); err != nil {
		return Upload{}, err
	}

	// The URL dies before the blob does. A write primitive should be usable for as long as
	// the client needs to perform one PUT and no longer, while the blob has to survive until
	// the message that carries it is sent.
	until := now.Add(uploadWindow)
	if until.After(expires) {
		until = expires
	}
	token := s.signer.Token(Claims{
		Use: UseUpload, BlobID: ref.ID, Owner: ref.Owner, Grant: ref.GrantID,
		Expires: until, Max: limit,
	})
	return Upload{
		Ref:        ref,
		URL:        s.baseURL + uploadPath + token,
		Method:     "PUT",
		MaxBytes:   limit,
		URLExpires: until,
	}, nil
}

// reserve refuses a write that would take storage past a cap, after clearing whatever has
// expired.
//
// Refusing rather than evicting something live is the deliberate half. Evicting the oldest
// unexpired blob to make room would break a link somebody is holding and about to use, and it
// would do so silently — a 404 on a URL that was valid when it was handed over is a far worse
// failure than a tool call that says the store is full.
func (s *Store) reserve(ctx context.Context, owner user.ID, size int64) error {
	if size > MaxUpload {
		return fmt.Errorf("%w: %d bytes is over the %d MiB ceiling", ErrTooLarge, size, MaxUpload>>20)
	}
	if _, err := s.Sweep(ctx); err != nil {
		return err
	}
	if s.opts.OwnerQuota > 0 {
		used, err := s.catalog.OwnerBlobBytes(ctx, owner)
		if err != nil {
			return err
		}
		if used+size > s.opts.OwnerQuota {
			return fmt.Errorf("%w: you are using %d MiB of a %d MiB attachment allowance. "+
				"Staged attachments are deleted automatically; wait for them to expire or send fewer at once",
				ErrQuota, used>>20, s.opts.OwnerQuota>>20)
		}
	}
	if s.opts.InstanceCap > 0 {
		total, err := s.catalog.TotalBlobBytes(ctx)
		if err != nil {
			return err
		}
		if total+size > s.opts.InstanceCap {
			return fmt.Errorf("%w: this instance's %d MiB attachment cache is full",
				ErrQuota, s.opts.InstanceCap>>20)
		}
	}
	return nil
}

// Receive writes the body of an upload, enforcing the ceiling as it goes.
//
// The limit is applied to the reader rather than to Content-Length, because Content-Length is
// whatever the client wrote in it and a chunked body carries none at all. Going over aborts
// the write and removes the partial blob: a truncated file that looks like a whole one is the
// one outcome worse than a refused upload.
func (s *Store) Receive(ctx context.Context, c Claims, body io.Reader) (Ref, error) {
	ref, err := s.catalog.ClaimBlob(ctx, c.Owner, c.BlobID, s.now())
	if err != nil {
		return Ref{}, err
	}

	limit := c.Max
	if limit <= 0 || limit > ref.Reserved {
		limit = ref.Reserved
	}

	written, err := s.bytes.Put(ctx, ref.ID, body, limit)
	if err != nil {
		_ = s.drop(ctx, ref)
		return Ref{}, err
	}
	if err := s.catalog.CompleteBlob(ctx, ref.Owner, ref.ID, written); err != nil {
		if rmErr := s.bytes.Delete(ctx, ref.ID); rmErr != nil {
			s.log.Error("an upload could not be completed and its bytes would not roll back",
				"blob", ref.ID, "err", rmErr)
		}
		return Ref{}, err
	}

	ref.State = StateReady
	ref.Size = written
	return ref, nil
}

// Open returns the bytes of a ready blob, having checked it has not expired.
func (s *Store) Open(ctx context.Context, owner user.ID, id string) (io.ReadCloser, int64, error) {
	ref, err := s.Ref(ctx, owner, id)
	if err != nil {
		return nil, 0, err
	}
	if ref.State != StateReady {
		return nil, 0, ErrNotReady
	}
	return s.bytes.Open(ctx, id)
}

// Ref reads a blob's metadata, treating an expired one as gone and deleting it on the way
// past. Expiry is enforced on read as well as by the sweeper so that a blob is never served
// after its time even if the sweeper is not running.
func (s *Store) Ref(ctx context.Context, owner user.ID, id string) (Ref, error) {
	ref, err := s.catalog.Blob(ctx, owner, id)
	if err != nil {
		return Ref{}, err
	}
	if !s.now().Before(ref.ExpiresAt) {
		// Expired is expired whether or not the bytes went. drop logs a failure and leaves
		// the row for the next sweep to retry; serving the blob because its file would not
		// delete is the one answer that is never acceptable.
		_ = s.drop(ctx, ref)
		return Ref{}, ErrGone
	}
	return ref, nil
}

// Content reads a whole blob into memory, for the send path where the bytes have to be
// assembled into a message anyway. Bounded by MaxUpload, which is the ceiling every write
// went through.
func (s *Store) Content(ctx context.Context, owner user.ID, id string) (Ref, []byte, error) {
	ref, err := s.Ref(ctx, owner, id)
	if err != nil {
		return Ref{}, nil, err
	}
	if ref.State != StateReady {
		return Ref{}, nil, ErrNotReady
	}
	rc, _, err := s.bytes.Open(ctx, id)
	if err != nil {
		return Ref{}, nil, err
	}
	defer rc.Close()

	content, err := io.ReadAll(io.LimitReader(rc, MaxUpload+1))
	if err != nil {
		return Ref{}, nil, err
	}
	if int64(len(content)) > MaxUpload {
		return Ref{}, nil, ErrTooLarge
	}
	return ref, content, nil
}

// Sweep deletes everything past its expiry, bytes first, and returns how many blobs it
// actually removed.
//
// A blob whose bytes will not delete is left whole and not counted, so the next pass finds it
// again. That failure is reported through the log rather than through this error on purpose:
// reserve sweeps before every write, and failing an upload because some unrelated blob's file
// is stuck would turn one bad file into a store nobody can write to. The error return is for
// the query, which is the one failure that makes the whole pass meaningless.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	expired, err := s.catalog.ExpiredBlobs(ctx, s.now())
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, ref := range expired {
		if s.drop(ctx, ref) == nil {
			dropped++
		}
	}
	return dropped, nil
}

// SweepEvery runs the sweeper until the context ends, starting with one pass immediately: a
// restart after downtime is exactly when the store is holding bytes that should already be
// gone.
func (s *Store) SweepEvery(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := s.Sweep(ctx); err != nil {
			s.log.Warn("could not sweep expired attachments", "err", err)
		} else if n > 0 {
			s.log.Info("deleted expired attachments", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drop removes a blob's bytes and then its row, and keeps the row if the bytes will not go.
//
// Bytes first: a row without a file is a confusing 500 for whoever holds the link, while a
// file without a row is invisible. Invisible is the worse of the two here, which is why a
// failed unlink stops the delete instead of being ignored. Nothing in this package walks the
// attachment directory — every deletion is driven from a row — so removing the row is
// removing the last thing that knows those bytes exist, and they would sit on the volume,
// uncounted against any quota, until somebody looked with their own eyes.
//
// Keeping the row costs nothing and buys a retry. The row is already past its expiry, so Ref
// still refuses to serve it and Sweep picks it up again on the next pass; the owner goes on
// being charged for disk that really is still in use, which is the honest accounting. A file
// that is simply not there is not a failure — Dir.Delete reports a missing key as success —
// so this only fires on a volume that is refusing, and that is the operator's to fix.
func (s *Store) drop(ctx context.Context, ref Ref) error {
	if err := s.bytes.Delete(ctx, ref.ID); err != nil {
		s.log.Error("could not delete an expired attachment's bytes; keeping its row so the next sweep retries",
			"blob", ref.ID, "err", err)
		return err
	}
	if err := s.catalog.DeleteBlob(ctx, ref.Owner, ref.ID); err != nil {
		s.log.Error("deleted an expired attachment's bytes but not its row; the row is charged for disk that is free",
			"blob", ref.ID, "err", err)
		return err
	}
	return nil
}

func (s *Store) downloadURL(ref Ref) string {
	return s.baseURL + downloadPath + s.signer.Token(Claims{
		Use: UseDownload, BlobID: ref.ID, Owner: ref.Owner,
		Grant: ref.GrantID, Expires: ref.ExpiresAt,
	})
}

// DownloadURL mints a link for a blob that already exists, which is how the upload route
// tells a client where to read back what it just wrote.
func (s *Store) DownloadURL(ref Ref) string { return s.downloadURL(ref) }

func normalizeType(t string) string {
	if t = strings.TrimSpace(t); t != "" {
		return t
	}
	return "application/octet-stream"
}

// SafeFilename reduces whatever a mail provider or a caller supplied to something that can
// only ever be a filename.
//
// It is not decoration. The name is echoed in a Content-Disposition header and, in the local
// backend, would otherwise be a candidate for a path — so a separator, a traversal segment or
// a bare control character has to stop being one here rather than at each use.
func SafeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '_'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "attachment"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
