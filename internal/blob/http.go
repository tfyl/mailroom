package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
)

// The two routes share a prefix so that a deployment behind an authenticating proxy has one
// path to exempt rather than two. Both are reached by a client with no browser session — that
// is the entire point of them — so they sit outside the operator guard and are authorised by
// their signature alone.
const (
	routePrefix  = "/attachments/"
	downloadPath = routePrefix
	uploadPath   = routePrefix + "upload/"
)

// Grants re-reads the grant a link was minted under. This is what makes revocation mean
// something on a URL that carries no bearer token.
type Grants interface {
	Grant(ctx context.Context, id grant.ID) (*grant.Grant, error)
}

// Auditor records byte delivery, which is a different event from the tool call that minted
// the link and may happen much later, or more than once.
type Auditor interface {
	Record(ctx context.Context, e grant.Audit) error
}

type Server struct {
	store  *Store
	grants Grants
	audit  Auditor
	log    *slog.Logger
	now    func() time.Time
}

func NewServer(store *Store, grants Grants, audit Auditor, log *slog.Logger) *Server {
	return &Server{store: store, grants: grants, audit: audit, log: log, now: time.Now}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+downloadPath+"{token}", s.download)
	mux.HandleFunc("PUT "+uploadPath+"{token}", s.upload)
}

// download serves a blob to whoever holds a valid link.
//
// Order matters and is the security property. The signature is verified first, so nothing a
// forged token claims is ever acted on or reflected back. Only then is the blob looked up —
// scoped to the owner the signature names, so a genuine signature over another owner's blob
// id finds nothing. Only then is the grant re-read.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	claims, ref, g, ok := s.resolve(w, r, UseDownload)
	if !ok {
		return
	}

	body, size, err := s.store.Open(r.Context(), claims.Owner, ref.ID)
	if err != nil {
		s.fail(w, UseDownload, 0, err)
		return
	}
	defer body.Close()

	// Recorded before a byte is written, and refused if it cannot be. The bytes have not
	// reached the client yet, so withholding them is what keeps "no mail leaves here
	// unrecorded" true rather than aspirational — the same rule the read tools follow, and
	// this route is where the content actually crosses the wire.
	if err := s.record(r.Context(), g, ref, "mail.attachment_download", "ok"); err != nil {
		s.log.Error("could not record an attachment download", "blob", ref.ID, "err", err)
		http.Error(w, "this download is being withheld because it could not be written to the audit log",
			http.StatusServiceUnavailable)
		return
	}

	harden(w, ref, size)
	if _, err := io.Copy(w, body); err != nil {
		// The status is already sent, so there is nowhere to report this but the log.
		s.log.Warn("an attachment download was cut short", "blob", ref.ID, "err", err)
	}
}

// upload accepts the body a client PUTs to a minted URL.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	claims, ref, g, ok := s.resolve(w, r, UseUpload)
	if !ok {
		return
	}
	if ref.Kind != KindUpload {
		http.Error(w, "this link is a download link for an attachment already in a mailbox, "+
			"not somewhere to write. Call mail_upload_url for an upload URL.",
			http.StatusNotFound)
		return
	}

	// The ceiling travels into the refusal so that a body over it is told what would have fit.
	// It is the signature's, narrowed by the reservation, which is what Receive enforces.
	limit := claims.Max
	if limit <= 0 || limit > ref.Reserved {
		limit = ref.Reserved
	}

	stored, err := s.store.Receive(r.Context(), claims, r.Body)
	if err != nil {
		s.fail(w, UseUpload, limit, err)
		return
	}
	if err := s.record(r.Context(), g, stored, "mail.attachment_upload", "ok"); err != nil {
		// Unlike a download, refusing here would report a failure that did not happen: the
		// bytes are already on disk. The blob is dropped instead, so the state the audit log
		// missed does not survive the request.
		s.log.Error("could not record an attachment upload", "blob", stored.ID, "err", err)
		s.store.drop(r.Context(), stored)
		http.Error(w, "this upload was discarded because it could not be written to the audit log",
			http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "{\n  %q: %q,\n  %q: %q,\n  %q: %d,\n  %q: %q,\n  %q: %q\n}\n",
		"blob_id", stored.ID,
		"filename", stored.Filename,
		"size_bytes", stored.Size,
		"expires_at", stored.ExpiresAt.UTC().Format(time.RFC3339),
		"download_url", s.store.DownloadURL(stored))
}

// resolve runs every check both routes share and writes the refusal itself when one fails.
//
// Keeping them in one place is deliberate: a second copy of this sequence is how one route
// ends up checking revocation and the other does not.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, use Use) (Claims, Ref, *grant.Grant, bool) {
	claims, err := s.store.signer.Parse(r.PathValue("token"), s.now())
	if err != nil {
		s.fail(w, use, 0, err)
		return Claims{}, Ref{}, nil, false
	}
	if claims.Use != use {
		// A link signed for reading is not permission to write, and the reverse. Both are
		// genuine signatures, so this is the check that keeps them apart.
		http.Error(w, "this link is not for this route: a download link cannot be written to "+
			"and an upload URL does not serve anything. Call "+minter(use)+" to get the right "+
			"one.", http.StatusForbidden)
		return Claims{}, Ref{}, nil, false
	}

	ref, err := s.store.Ref(r.Context(), claims.Owner, claims.BlobID)
	if err != nil {
		s.fail(w, use, claims.Max, err)
		return Claims{}, Ref{}, nil, false
	}

	g, err := s.authorize(r.Context(), claims, ref)
	if err != nil {
		s.fail(w, use, claims.Max, err)
		return Claims{}, Ref{}, nil, false
	}
	return claims, ref, g, true
}

// errRefused is every way a verified link can still be refused. They collapse to one status
// on purpose: the holder of a valid link learns that it no longer works, and not which of
// several changes to somebody else's grant is the reason.
var errRefused = errors.New("this link is no longer authorized")

// authorize re-reads the grant a link was minted under and checks it still permits this.
//
// This is the answer to the question a signed URL always raises: does revocation win, or does
// the signature run to its own expiry? Here revocation wins, without exception. Every other
// path in this server re-reads the grant on every request — the MCP endpoint says so in as
// many words — and a link that outlived a revocation would be the one place where an operator
// pressing Revoke did not stop the client reading their mail. That the link carries no bearer
// token makes it worse rather than better: it is a credential sitting in a transcript, which
// is precisely the sort that gets revoked.
//
// It costs a database read per fetch. On a single-instance SQLite deployment that is nothing,
// and it is the difference between "revoked" meaning revoked and meaning "in fifteen minutes".
//
// Narrowing counts as revoking. Editing a grant to drop the mailbox, or to drop the
// capability, kills its outstanding links too, because the consent screen presents those as
// the same decision.
func (s *Server) authorize(ctx context.Context, c Claims, ref Ref) (*grant.Grant, error) {
	if ref.Owner != c.Owner || ref.GrantID != c.Grant {
		return nil, errRefused
	}

	g, err := s.grants.Grant(ctx, c.Grant)
	if err != nil {
		return nil, errRefused
	}
	if g.OwnerID != ref.Owner {
		return nil, errRefused
	}
	if err := g.Valid(s.now()); err != nil {
		return nil, errRefused
	}
	// A mail copy is still that mailbox's mail, so the grant has to still cover the mailbox.
	// An upload came from no mailbox and has none to check.
	if ref.Kind == KindMail && !g.HasAccount(ref.AccountID) {
		return nil, errRefused
	}

	for _, want := range ref.Requires() {
		if g.Caps.Has(want) {
			return g, nil
		}
	}
	return nil, errRefused
}

func (s *Server) record(ctx context.Context, g *grant.Grant, ref Ref, tool, outcome string) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.Record(ctx, grant.Audit{
		OwnerID: g.OwnerID, GrantID: g.ID, AccountID: ref.AccountID,
		Tool: tool, Outcome: outcome, At: s.now(),
	})
}

// minter names the tool that hands out a link for this route. Every refusal below ends by
// naming it, because "this link no longer works" is only half an answer to an agent: the other
// half is which call produces a working one.
func minter(use Use) string {
	if use == UseUpload {
		return "mail_upload_url"
	}
	return "mail_get_attachment"
}

// fail maps an error to a status without saying more than the holder of the link already
// knows. Expiry and refusal are distinguished because they tell a client different things to
// do next — mint another link, versus stop — and neither discloses anything: the expiry is
// written in the token the client is holding, and the grant is its own.
//
// The text is written for whoever reads it next, and on this route that is usually an agent
// deciding what to try. So each case names the action that follows rather than only the state
// that was reached: an expired upload URL says to mint another, a body over the ceiling says
// the ceiling and that resending the same file will not help, and a link refused after its
// grant was narrowed says the access changed rather than implying the file is gone. The one
// thing none of them does is say more about somebody else's grant than the holder already
// knows.
//
// limit is the ceiling the signature carried, and is zero on any path where there is not one.
func (s *Server) fail(w http.ResponseWriter, use Use, limit int64, err error) {
	switch {
	case errors.Is(err, ErrExpired), errors.Is(err, ErrGone):
		if use == UseUpload {
			http.Error(w, "this upload URL has expired. It is usable for a few minutes only, "+
				"and nothing was written through it. Call mail_upload_url again for a fresh "+
				"URL and blob_id, then PUT the file to that one.", http.StatusGone)
			return
		}
		http.Error(w, "this download link has expired. The attachment is still in the mailbox "+
			"— only this server's short-lived copy of it has gone. Call mail_get_attachment "+
			"again for a fresh link.", http.StatusGone)
	case errors.Is(err, errRefused):
		// Deliberately about the access rather than about the file. The blob is very likely
		// still on disk; what changed is the grant, and a client told "not found" would go
		// looking for the message again instead of asking for permission back.
		http.Error(w, "this link is no longer authorized: the grant it was minted under has "+
			"been revoked, has expired, or no longer covers this mailbox or capability. The "+
			"attachment has not gone anywhere. Ask whoever approved this connection to restore "+
			"the access, then call "+minter(use)+" again — an old link cannot be revived.",
			http.StatusForbidden)
	case errors.Is(err, ErrClaimed):
		http.Error(w, "this upload URL has already been used. Each one accepts exactly one "+
			"PUT, and there is no way to overwrite or resume what it wrote. If the first PUT "+
			"carried the wrong bytes, call mail_upload_url again and use the new blob_id.",
			http.StatusConflict)
	case errors.Is(err, ErrTooLarge):
		http.Error(w, "the body is larger than this upload URL allows"+ceiling(limit)+". It "+
			"was refused part-way through writing and nothing was kept, so PUTting the same "+
			"file again will fail in the same place. Send a smaller file, or split it.",
			http.StatusRequestEntityTooLarge)
	case errors.Is(err, ErrNotReady):
		http.Error(w, "nothing has been uploaded here yet: this blob_id was reserved by "+
			"mail_upload_url, but no PUT ever completed against its URL. Upload the bytes "+
			"before naming the blob_id.", http.StatusNotFound)
	case errors.Is(err, ErrQuota):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case errors.Is(err, ErrMalformed), errors.Is(err, ErrSignature), errors.Is(err, ErrNotFound):
		// One answer for a bad signature, a mangled token and an id that does not exist. A
		// caller holding a real link never sees any of the three, and separating them would
		// let somebody probing tell a signature failure from a miss.
		http.Error(w, "no such attachment: this link does not name anything this server is "+
			"holding. Do not retry it — call "+minter(use)+" for one that does.",
			http.StatusNotFound)
	default:
		s.log.Error("serving an attachment failed", "err", err)
		http.Error(w, "could not serve this attachment", http.StatusInternalServerError)
	}
}

// ceiling renders the limit as a clause, and renders nothing when there is no number to give.
// A refusal that says "over the limit" without saying the limit leaves a caller guessing at
// what would fit.
func ceiling(limit int64) string {
	if limit <= 0 {
		return ""
	}
	if limit >= 1<<20 {
		return fmt.Sprintf(" (%d MiB)", limit>>20)
	}
	return fmt.Sprintf(" (%d bytes)", limit)
}

// harden writes the response headers, and every one of them is load-bearing.
//
// The threat is specific. An uploaded blob is bytes a client chose, served back from
// mailroom's own origin — the origin holding the operator's session cookie. Somebody who
// uploads an HTML or SVG file and opens its download link would be running script there. The
// page CSP does not help: it applies to the app's own documents, not to a raw response.
//
//   - The Content-Type is never the caller's. It is checked against a list of types no
//     browser executes, and anything not on it — text/html, image/svg+xml, anything XML,
//     anything unrecognised — is served as application/octet-stream.
//   - Content-Disposition is always `attachment`, never `inline`, including for PDFs, where
//     inline would have been the friendlier choice. The fetcher here is an HTTP client rather
//     than a person with a browser, so inline buys almost nothing, and a viewer rendering
//     third-party content in this origin is not worth almost nothing.
//   - nosniff stops a browser deciding for itself that octet-stream was really HTML.
//   - A per-response CSP of `default-src 'none'; sandbox` denies the document any origin at
//     all, should anything render it regardless.
//   - no-store, because a cached copy would outlive the signature that authorised it.
func harden(w http.ResponseWriter, ref Ref, size int64) {
	h := w.Header()
	h.Set("Content-Type", ServableType(ref.MimeType))
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Content-Disposition", disposition(ref.Filename))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-store, max-age=0")
}

// inertTypes are the media types this server will echo back. The test is not "is this type
// harmless" but "can a browser be talked into executing it in our origin": everything here
// renders as data or downloads, and nothing here carries script or can be coerced into a
// document. Anything absent is served as bytes, which costs a caller nothing — the filename
// still says what the file is — and closes the question for every type nobody has considered.
var inertTypes = map[string]bool{
	"application/json":              true,
	"application/octet-stream":      true,
	"application/pdf":               true,
	"application/rtf":               true,
	"application/zip":               true,
	"application/gzip":              true,
	"application/msword":            true,
	"application/vnd.ms-excel":      true,
	"application/vnd.ms-powerpoint": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	"application/vnd.oasis.opendocument.text":                                   true,
	"application/vnd.oasis.opendocument.spreadsheet":                            true,
	"text/plain":     true,
	"text/csv":       true,
	"text/calendar":  true,
	"text/markdown":  true,
	"image/png":      true,
	"image/jpeg":     true,
	"image/gif":      true,
	"image/webp":     true,
	"image/bmp":      true,
	"image/tiff":     true,
	"image/avif":     true,
	"audio/mpeg":     true,
	"audio/ogg":      true,
	"audio/wav":      true,
	"video/mp4":      true,
	"video/webm":     true,
	"message/rfc822": true,
}

// ServableType is the Content-Type this server is willing to put on a blob response.
func ServableType(declared string) string {
	base, _, err := mime.ParseMediaType(declared)
	if err != nil {
		return "application/octet-stream"
	}
	base = strings.ToLower(strings.TrimSpace(base))
	if !inertTypes[base] {
		return "application/octet-stream"
	}
	// Parameters are dropped along with everything else the caller wrote. A charset on a
	// text/plain response is not worth a second thing to have got right.
	return base
}

func disposition(filename string) string {
	name := SafeFilename(filename)
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return `attachment; filename="attachment"`
}
