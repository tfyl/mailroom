package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// The held-action queue. Every method takes the owner explicitly, like everything else here:
// an unscoped query would be one missed check away from letting somebody approve a message
// composed in another person's mailbox.

func (s *Store) HoldAction(ctx context.Context, owner user.ID, a held.Action) error {
	if owner == "" {
		return errors.New("a held action must have an owner")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO held_actions (id, owner_id, grant_id, account_id, tool, kind, summary, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, string(owner), string(a.GrantID), string(a.AccountID),
		a.Tool, string(a.Kind), a.Summary, string(a.Payload), unix(a.CreatedAt))
	return err
}

// heldColumns is the projection every read below shares, joined out to the two things the
// page needs and this table does not hold: the grant's label, and the mailbox's alias.
//
// Both come from a LEFT JOIN so that a held action outlives whatever it names. A grant
// revoked, or a mailbox unlinked, after the action was queued must still render — that is
// precisely the state in which somebody wants to look at what the client had asked for.
const heldColumns = `
	h.id, h.owner_id, h.grant_id, h.account_id, h.tool, h.kind, h.summary, h.payload,
	h.created_at, h.resolved_at, h.resolution,
	COALESCE(g.label, ''), COALESCE(g.revoked_at, 0), COALESCE(acc.alias, h.account_id)
	FROM held_actions h
	LEFT JOIN grants g ON g.id = h.grant_id
	LEFT JOIN accounts acc ON acc.id = h.account_id AND acc.owner_id = h.owner_id`

func scanHeld(row rowScanner) (held.Action, error) {
	var (
		a          held.Action
		owner      string
		grantID    string
		accountID  string
		kind       string
		payload    string
		created    int64
		resolved   sql.NullInt64
		revokedAt  int64
		grantLabel string
		alias      string
	)
	err := row.Scan(&a.ID, &owner, &grantID, &accountID, &a.Tool, &kind, &a.Summary, &payload,
		&created, &resolved, &a.Resolution, &grantLabel, &revokedAt, &alias)
	if errors.Is(err, sql.ErrNoRows) {
		return held.Action{}, held.ErrNotPending
	}
	if err != nil {
		return held.Action{}, err
	}
	a.OwnerID = user.ID(owner)
	a.GrantID = grant.ID(grantID)
	a.AccountID = mail.AccountID(accountID)
	a.Kind = held.Kind(kind)
	a.Payload = []byte(payload)
	a.CreatedAt = fromUnix(created)
	a.ResolvedAt = timePtr(resolved)
	a.GrantLabel, a.GrantRevoked, a.Account = grantLabel, revokedAt != 0, alias
	return a, nil
}

// stillWaiting is the pending test every read here shares: unanswered, and not yet past the
// cutoff its caller computed from the TTL. Written once and reused so that the listing, the
// count against the per-grant cap and the claim cannot drift apart into a row that is shown
// but not answerable, or answerable but not shown.
//
// A zero cutoff arrives as 0, and no row was created at or before the epoch, so retention
// turned off reads as the unbounded behaviour this table used to have.
const stillWaiting = ` h.resolved_at IS NULL AND h.created_at > ? `

func (s *Store) PendingActions(ctx context.Context, owner user.ID, cutoff time.Time) ([]held.Action, error) {
	// Oldest first, which is the opposite of every other list in the product and right for
	// this one: this is a queue somebody works through, not a history they scan.
	//
	// The id breaks a tie, because created_at is unix seconds and a client queueing a batch
	// puts several rows inside one of them. A held id is millisecond-ordered and sorts
	// lexicographically (see internal/ids), so it is the finer clock this column rounded off.
	return s.heldQuery(ctx, `SELECT`+heldColumns+`
		WHERE h.owner_id = ? AND`+stillWaiting+`
		ORDER BY h.created_at ASC, h.id ASC`, string(owner), unix(cutoff))
}

func (s *Store) RecentActions(ctx context.Context, owner user.ID, limit int) ([]held.Action, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.heldQuery(ctx, `SELECT`+heldColumns+`
		WHERE h.owner_id = ? AND h.resolved_at IS NOT NULL
		ORDER BY h.resolved_at DESC, h.id DESC LIMIT ?`, string(owner), limit)
}

func (s *Store) heldQuery(ctx context.Context, query string, args ...any) ([]held.Action, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []held.Action
	for rows.Next() {
		a, err := scanHeld(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CountPending(ctx context.Context, owner user.ID, id grant.ID, cutoff time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM held_actions h
		WHERE h.owner_id = ? AND h.grant_id = ? AND`+stillWaiting,
		string(owner), string(id), unix(cutoff)).Scan(&n)
	return n, err
}

// ExpireActions reclaims every unanswered action at or before the cutoff.
//
// It is the same three-column write ClaimAction makes, and that is the point rather than a
// coincidence: expiry is a resolution, not a deletion. The payload — the composed message,
// its attachment bytes — goes, and the row stays as the summary, the grant that asked and
// the time it gave up. Deleting it instead would lose the record that a client asked for
// this at all, which is the half worth keeping; the mail is the half worth destroying.
//
// Because it writes resolved_at, it is also what makes an expired action unanswerable: every
// path to a payload is conditional on that column being NULL.
//
// Not owner-scoped, for the reason ExpiredBlobs is not: a per-owner sweep would need a list
// of owners to be complete.
func (s *Store) ExpireActions(ctx context.Context, cutoff time.Time) (int, error) {
	at := unix(cutoff)
	if at <= 0 {
		// Retention is off. Guarded here as well as in the caller because a bug that let a
		// zero cutoff through would otherwise expire nothing, silently, and look like it
		// worked — and the opposite mistake, a cutoff of "now", would empty the queue.
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE held_actions SET resolved_at = ?, resolution = ?, payload = ''
		WHERE resolved_at IS NULL AND created_at <= ?`,
		unix(time.Now()), held.Expired, at)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ClaimAction takes one pending action out of the queue and hands back what it held.
//
// The read and the write are one transaction, and the write is conditional on the row still
// being unresolved. That pairing is the whole double-approval defence: two tabs pressing
// Approve, or a form resubmitted from the browser's history, both reach this, and the UPDATE
// matches for exactly one of them. The loser gets ErrNotPending and no payload, so a message
// cannot leave the mailbox twice.
//
// The payload is cleared as it is claimed. It is the one place in this database that holds
// message bodies and attachment bytes, and it holds them only for as long as the instruction
// is still waiting to be carried out.
//
// The cutoff is in both statements. An action past it is expired, and an expired action is
// answered by nobody: the SELECT does not find it, and were it found some other way the
// UPDATE would still match no rows. Putting the rule in the conditional write rather than in
// a check above it is deliberate — this is the single statement every approval and every
// discard goes through, so there is no second path for an expiry test to be missing from.
func (s *Store) ClaimAction(ctx context.Context, owner user.ID, id, resolution string, cutoff time.Time) (held.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return held.Action{}, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `SELECT`+heldColumns+`
		WHERE h.id = ? AND h.owner_id = ? AND`+stillWaiting, id, string(owner), unix(cutoff))
	a, err := scanHeld(row)
	if err != nil {
		// Somebody else's action, an id that never existed, and one already answered all
		// report the same way. Telling them apart would confirm that an id is real.
		return held.Action{}, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE held_actions SET resolved_at = ?, resolution = ?, payload = ''
		WHERE id = ? AND owner_id = ? AND resolved_at IS NULL AND created_at > ?`,
		unix(time.Now()), resolution, id, string(owner), unix(cutoff))
	if err != nil {
		return held.Action{}, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return held.Action{}, held.ErrNotPending
	}
	if err := tx.Commit(); err != nil {
		return held.Action{}, err
	}

	a.Resolution = resolution
	now := time.Now()
	a.ResolvedAt = &now
	return a, nil
}

// MarkFailed rewrites the resolution of an action this server has already claimed, when
// carrying it out then failed. It deliberately does not put the row back: see held.Approve
// for why a failed send stays out of the queue rather than becoming approvable again.
func (s *Store) MarkFailed(ctx context.Context, owner user.ID, id, reason string) error {
	if len(reason) > 300 {
		reason = reason[:300] + "…"
	}
	return s.affectOne(ctx, `
		UPDATE held_actions SET resolution = ?
		WHERE id = ? AND owner_id = ? AND resolved_at IS NOT NULL`,
		held.Failed+": "+reason, id, string(owner))
}
