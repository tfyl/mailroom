package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// --- Clients ---

type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

func (s *Store) RegisterClient(ctx context.Context, c Client) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Name, strings.Join(c.RedirectURIs, " "), unix(time.Now()))
	return err
}

func (s *Store) Client(ctx context.Context, id string) (Client, error) {
	var c Client
	var uris string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, redirect_uris, created_at FROM clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, mail.ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	c.RedirectURIs = strings.Fields(uris)
	c.CreatedAt = fromUnix(created)
	return c, nil
}

// --- Grants ---

// CreateGrant records an approved grant.
//
// Every account named must already belong to the owner. The consent screen only offers their
// mailboxes, so this is a second check rather than the only one — but a grant is the thing
// an MCP client presents later, and one naming a mailbox its owner does not own would be a
// standing hole.
func (s *Store) CreateGrant(ctx context.Context, g *grant.Grant) error {
	if g.OwnerID == "" {
		return fmt.Errorf("a grant must have an owner")
	}
	for _, id := range g.Accounts {
		if _, err := s.Account(ctx, g.OwnerID, id); err != nil {
			return fmt.Errorf("account %s is not yours to grant", id)
		}
	}

	accounts := make([]string, len(g.Accounts))
	for i, a := range g.Accounts {
		accounts[i] = string(a)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO grants (id, owner_id, client_id, label, accounts, capabilities, mode, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(g.ID), string(g.OwnerID), g.ClientID, g.Label,
		strings.Join(accounts, ","), g.Caps.String(), string(g.Mode),
		unix(time.Now()), nullTime(g.ExpiresAt))
	return err
}

// Grant loads a grant by id. Unscoped by design: it is reached through a bearer token that
// already proves possession, and the grant's own OwnerID is what scopes everything it can
// then do.
//
// A removed grant is not there. It was revoked before it could be removed, so every caller
// here already refused it — but a lookup that still resolved one would leave the row visible
// to the token path and invisible on the page, and the two should not be able to disagree.
func (s *Store) Grant(ctx context.Context, id grant.ID) (*grant.Grant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, client_id, label, accounts, capabilities, mode, created_at, expires_at, last_used_at, revoked_at
		FROM grants WHERE id = ? AND deleted_at IS NULL`, string(id))
	return scanGrant(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGrant(row rowScanner) (*grant.Grant, error) {
	var (
		g                    grant.Grant
		owner                sql.NullString
		accounts, caps, mode string
		created              int64
		expires, used, revd  sql.NullInt64
	)
	err := row.Scan(&g.ID, &owner, &g.ClientID, &g.Label, &accounts, &caps, &mode, &created, &expires, &used, &revd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, grant.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	g.OwnerID = user.ID(owner.String)
	for _, a := range strings.Split(accounts, ",") {
		if a = strings.TrimSpace(a); a != "" {
			g.Accounts = append(g.Accounts, mail.AccountID(a))
		}
	}
	// A capability stored by a newer version and unknown here must not be silently dropped
	// into a narrower grant that still looks valid.
	set, err := mail.ParseSet(caps)
	if err != nil {
		return nil, err
	}
	g.Caps = set
	// Stored as it was written, not as it resolves. An empty column means nobody has chosen a
	// mode for this grant, and grant.Mode's own methods answer for it — normalising here
	// instead would make a default indistinguishable from a choice, which is exactly the
	// difference the edit page has to show.
	g.Mode = grant.Mode(mode)
	g.CreatedAt = fromUnix(created)
	g.ExpiresAt, g.LastUsedAt, g.RevokedAt = timePtr(expires), timePtr(used), timePtr(revd)
	return &g, nil
}

func (s *Store) ListGrants(ctx context.Context, owner user.ID) ([]*grant.Grant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, client_id, label, accounts, capabilities, mode, created_at, expires_at, last_used_at, revoked_at
		FROM grants WHERE owner_id = ? AND deleted_at IS NULL ORDER BY created_at DESC`, string(owner))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*grant.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant is immediate: every token referencing the grant stops working on its next use,
// because tokens carry only a grant id and are resolved on each call.
//
// Scoped to the owner so that knowing, or guessing, another user's grant id is not enough to
// revoke it.
func (s *Store) RevokeGrant(ctx context.Context, owner user.ID, id grant.ID) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE grants SET revoked_at = ? WHERE id = ? AND owner_id = ? AND revoked_at IS NULL`,
		unix(time.Now()), string(id), string(owner))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return grant.ErrNotFound
	}
	return nil
}

// RemoveGrant takes one revoked grant off its owner's page.
//
// It is a soft delete, and the audit log is the reason. audit_log.grant_id carries no foreign
// key and RecentAudit resolves each row's name with a LEFT JOIN onto this table, so deleting
// the row here would keep every historical row and blank the grant name on all of them: the
// history would survive and stop being readable, which is the opposite of what the audit page
// is for. The row that stays is not a grant any more — nothing loads it, no token resolves to
// it, and it appears nowhere in the UI — it is the name its own audit rows are read under.
//
// Only a revoked grant qualifies, which is the predicate rather than a check the caller can
// forget. A live grant is removable only by revoking it first, which is the step that asks
// and explains what breaks. An expired grant does not qualify either: it reaches nothing
// today, but it is a single edit — a new expiry — from working again, so removing one would
// end an authorisation without ever asking the question revoking asks.
func (s *Store) RemoveGrant(ctx context.Context, owner user.ID, id grant.ID) error {
	n, err := s.removeRevoked(ctx, owner, ` AND id = ?`, string(id))
	if err != nil {
		return err
	}
	// Not there, not yours, not revoked: one answer for all three, because confirming that an
	// id is real but not yours is itself a disclosure.
	if n == 0 {
		return grant.ErrNotFound
	}
	return nil
}

// RemoveRevokedGrants clears the whole revoked band in one go, and reports how many it took.
//
// It removes nothing RemoveGrant could not remove one at a time: the predicate is the same,
// so a live or expired grant is not reachable through the bulk path either.
func (s *Store) RemoveRevokedGrants(ctx context.Context, owner user.ID) (int, error) {
	return s.removeRevoked(ctx, owner, "")
}

// removeRevoked marks the matching grants removed and clears what they left behind.
//
// One transaction, because the three writes are one act: a grant that was off the page while
// its tokens were still in the table would be a row nobody can see holding credentials
// somebody could still present.
//
// The ids are read first and then acted on individually rather than by re-running the
// predicate. The UPDATE is what makes the predicate stop matching, so a second statement
// using it would find nothing — and the tokens and blobs would be left behind by exactly the
// grants that were removed.
func (s *Store) removeRevoked(ctx context.Context, owner user.ID, extra string, args ...any) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	params := append([]any{string(owner)}, args...)
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM grants
		 WHERE owner_id = ? AND revoked_at IS NOT NULL AND deleted_at IS NULL`+extra, params...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	now := time.Now()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE grants SET deleted_at = ? WHERE id = ? AND owner_id = ?`,
			unix(now), id, string(owner)); err != nil {
			return 0, err
		}
		// The tokens are already dead — they resolve through the grant, which was revoked —
		// so this loses nothing, and it is what keeps tokens.grant_id from pointing at a row
		// no lookup will ever return.
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE grant_id = ?`, id); err != nil {
			return 0, err
		}
		// Blobs are expired rather than deleted here, because their bytes are on disk and
		// this package cannot reach them. Expiring the rows hands both halves to the sweeper,
		// which deletes the file before the row it is found by; deleting the row outright
		// would leave the bytes with nothing left to find them. A fetch is already refused —
		// every one of them re-reads the grant — and Store.Ref treats a past expiry as gone.
		if _, err := tx.ExecContext(ctx,
			`UPDATE blobs SET expires_at = ? WHERE grant_id = ? AND owner_id = ?`,
			now.Unix(), id, string(owner)); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

// EditGrant rewrites what a live grant reaches: its mailboxes, its capabilities, its mode and
// its expiry. Everything else about it — who owns it, which client holds it, when it was created
// — is fixed, and the tokens already issued against it are deliberately left alone. That is
// the point of the operation: a grant is corrected in place instead of being revoked and
// rebuilt, which would cost the client its token and need whoever runs it to authorise again.
//
// The mode is written here and nowhere else but CreateGrant, and both are reached only from a
// browser handler behind an authenticated session. There is deliberately no path to this
// column from the MCP endpoint: a client that could loosen its own mode would have no mode.
//
// The account check is the same one CreateGrant makes, for the same reason and with more
// force. On the consent screen the ids come from a list this server rendered; here they come
// from a form that names them explicitly, so a posted id is the obvious thing to try. Every
// one of them has to belong to the owner named in the call, and the owner named in the call
// is the signed-in operator rather than anything the form said.
//
// The update is scoped to the owner as well, and refuses a revoked grant. Editing a revoked
// grant could only mean bringing it back, and revocation is documented as the thing that
// cannot be undone.
func (s *Store) EditGrant(ctx context.Context, owner user.ID, id grant.ID, accounts []mail.AccountID, caps mail.Set, mode grant.Mode, expires *time.Time) error {
	if owner == "" {
		return fmt.Errorf("a grant must have an owner")
	}
	for _, a := range accounts {
		if _, err := s.Account(ctx, owner, a); err != nil {
			return fmt.Errorf("account %s is not yours to grant", a)
		}
	}

	ids := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = string(a)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE grants SET accounts = ?, capabilities = ?, mode = ?, expires_at = ?
		WHERE id = ? AND owner_id = ? AND revoked_at IS NULL`,
		strings.Join(ids, ","), caps.String(), string(mode), nullTime(expires),
		string(id), string(owner))
	if err != nil {
		return err
	}
	// Nothing updated means the grant is somebody else's, revoked, or not there at all. All
	// three are reported the same way: confirming that an id is real but not yours is itself
	// a disclosure.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return grant.ErrNotFound
	}
	return nil
}

// TouchGrant records that a client presented this grant.
//
// Owner-scoped like every other write here, even though the id arrives already proven by a
// bearer token: the owner is on the grant the token resolved to, so scoping it costs nothing
// and keeps this from being the one write a bare id is enough to reach.
//
// The time comes from the caller rather than from time.Now() because the caller has just
// decided, against a clock of its own, that the stored value is stale enough to replace.
// Deciding with one clock and writing with another is how a coarsening window drifts.
func (s *Store) TouchGrant(ctx context.Context, owner user.ID, id grant.ID, at time.Time) error {
	// A miss is not reported. It means the grant was deleted between the read and this
	// write, and there is nobody to tell: this is bookkeeping behind a call still in flight.
	_, err := s.db.ExecContext(ctx,
		`UPDATE grants SET last_used_at = ? WHERE id = ? AND owner_id = ?`,
		unix(at), string(id), string(owner))
	return err
}

// --- Tokens ---

// hashToken is what gets stored. The bearer token itself never touches the database, so a
// leaked copy of the file does not hand over working credentials.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) IssueToken(ctx context.Context, token string, id grant.ID, expires *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (token_hash, grant_id, issued_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), string(id), unix(time.Now()), nullTime(expires))
	return err
}

// GrantForToken resolves a bearer token to its grant, re-reading the grant on every call so
// that a revocation takes effect immediately rather than at the next token expiry.
func (s *Store) GrantForToken(ctx context.Context, token string) (*grant.Grant, error) {
	var grantID string
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT grant_id, expires_at FROM tokens WHERE token_hash = ?`, hashToken(token)).
		Scan(&grantID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, grant.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expires.Valid && time.Now().After(time.Unix(expires.Int64, 0)) {
		return nil, grant.ErrExpired
	}
	return s.Grant(ctx, grant.ID(grantID))
}

func (s *Store) RevokeTokensForGrant(ctx context.Context, owner user.ID, id grant.ID) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tokens WHERE grant_id IN (
			SELECT id FROM grants WHERE id = ? AND owner_id = ?
		)`, string(id), string(owner))
	return err
}

// --- Audit ---

// Record writes one call to the log.
//
// Bounded is applied here rather than at any of the call sites that build an entry, so that
// the caps on how much one row may carry are a property of writing a row rather than of
// remembering to. The detail is marshalled unconditionally, empty object included: `detail IS
// NULL` is then exactly "this row predates the detail columns", which is what the page needs
// in order to describe an old row honestly instead of drawing it with the facts missing.
func (s *Store) Record(ctx context.Context, e grant.Audit) error {
	e = e.Bounded()
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_log (owner_id, grant_id, account_id, tool, outcome, at,
		                       capability, reason, affected, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(e.OwnerID), string(e.GrantID), string(e.AccountID), e.Tool, e.Outcome, unix(e.At),
		string(e.Capability), e.Reason, nullInt(e.Affected), string(detail))
	return err
}

func nullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

type AuditEntry struct {
	GrantID   string
	GrantName string
	Account   string
	Tool      string
	Outcome   string
	At        time.Time

	Capability string
	Reason     string
	Affected   *int
	Detail     grant.Detail
	// Detailed is false for a row written before the log carried any of the above. The page
	// says so in words rather than rendering the row as though the call had nothing to
	// record, which would be a claim about the call rather than about the log.
	Detailed bool
}

func (s *Store) RecentAudit(ctx context.Context, owner user.ID, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	// The account join is scoped to the row's own owner as well as to the id. The id on a row
	// is not always one this server chose — a refused call is recorded with the mailbox it was
	// refused against — and an unscoped join would render another user's alias on this
	// operator's page for any id that happened to collide.
	//
	// Ordered by id within a second, and that tiebreaker is not decoration. `at` is unix
	// seconds, so most of a busy client's calls share one, and ordering by `at` alone let the
	// index decide what came next — which it does by ascending rowid, so rows inside the same
	// second came back oldest-first in a newest-first page. A tool call and the refusal it
	// provoked would be listed the wrong way round on the one page that exists to say what
	// happened in what order. The id is AUTOINCREMENT and monotonic, so it is exactly the
	// insertion order the timestamp is too coarse to carry.
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.grant_id, COALESCE(g.label, ''), COALESCE(acc.alias, a.account_id), a.tool, a.outcome, a.at,
		       COALESCE(a.capability, ''), COALESCE(a.reason, ''), a.affected, a.detail
		FROM audit_log a
		LEFT JOIN grants g ON g.id = a.grant_id
		LEFT JOIN accounts acc ON acc.id = a.account_id AND acc.owner_id = a.owner_id
		WHERE a.owner_id = ?
		ORDER BY a.at DESC, a.id DESC LIMIT ?`, string(owner), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var affected sql.NullInt64
		var detail sql.NullString
		if err := rows.Scan(&e.GrantID, &e.GrantName, &e.Account, &e.Tool, &e.Outcome, &at,
			&e.Capability, &e.Reason, &affected, &detail); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		if affected.Valid {
			n := int(affected.Int64)
			e.Affected = &n
		}
		if detail.Valid {
			e.Detailed = true
			// A row whose detail will not parse is still a real call that was really made, so
			// it is reported with its columns and without the JSON rather than failing the
			// whole page over one malformed value.
			_ = json.Unmarshal([]byte(detail.String), &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountSends returns how many sends a grant has made inside the window, backing the
// per-grant send rate limit.
func (s *Store) CountSends(ctx context.Context, id grant.ID, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE grant_id = ? AND tool = 'mail.send' AND outcome = 'ok' AND at >= ?`,
		string(id), unix(since)).Scan(&n)
	return n, err
}
