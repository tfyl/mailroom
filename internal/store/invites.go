package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tfyl/mailroom/internal/ids"
	"github.com/tfyl/mailroom/internal/signup"
	"github.com/tfyl/mailroom/internal/user"
)

// ErrNoInvite is returned when a code matches nothing usable — unknown, already redeemed,
// revoked, or expired. One error for all four on purpose: distinguishing them would tell
// whoever is guessing which codes exist.
var ErrNoInvite = errors.New("no usable invite")

// Invite is one issued invitation. The code itself is not here; only its hash is stored.
type Invite struct {
	ID         string
	Note       string
	CreatedBy  user.ID
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RedeemedBy user.ID
	RedeemedAt *time.Time
	RevokedAt  *time.Time
	// Adopts names an existing user this invite moves onto a new login, rather than the
	// account it would otherwise create. Empty for an ordinary invite.
	Adopts user.ID
}

// Usable reports whether this invite would still be accepted.
func (i Invite) Usable(now time.Time) bool {
	switch {
	case i.RedeemedAt != nil, i.RevokedAt != nil:
		return false
	case i.ExpiresAt != nil && now.After(*i.ExpiresAt):
		return false
	default:
		return true
	}
}

// State names the invite's condition for display.
func (i Invite) State(now time.Time) string {
	switch {
	case i.RedeemedAt != nil:
		return "redeemed"
	case i.RevokedAt != nil:
		return "revoked"
	case i.ExpiresAt != nil && now.After(*i.ExpiresAt):
		return "expired"
	default:
		return "open"
	}
}

// CreateInvite issues an invite and returns the code, which is the only time it exists in
// readable form.
func (s *Store) CreateInvite(ctx context.Context, by user.ID, note string, ttl time.Duration) (Invite, string, error) {
	code, err := signup.NewCode()
	if err != nil {
		return Invite{}, "", err
	}

	now := time.Now()
	inv := Invite{ID: ids.New("inv"), Note: note, CreatedBy: by, CreatedAt: now}
	if ttl > 0 {
		expires := now.Add(ttl)
		inv.ExpiresAt = &expires
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO invites (id, code_hash, note, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		inv.ID, signup.HashCode(code), inv.Note, string(by), unix(now), nullTime(inv.ExpiresAt)); err != nil {
		return Invite{}, "", err
	}
	return inv, code, nil
}

// CreateAdoptionInvite issues an invite that moves an existing user onto whichever login
// redeems it, instead of creating a new account.
//
// This is how an instance recovers when the provider that issued its original login is gone:
// the account keeps its id and therefore its mailboxes, grants and audit history, and simply
// answers to a different identity afterwards. It is minted from the command line rather than
// the UI on purpose, because the person who needs it is by definition unable to sign in — so
// the authorisation for it is having a shell on the host, which is already enough to read the
// database.
func (s *Store) CreateAdoptionInvite(ctx context.Context, adopt user.ID, ttl time.Duration) (Invite, string, error) {
	if adopt == "" {
		return Invite{}, "", ErrNoUser
	}
	code, err := signup.NewCode()
	if err != nil {
		return Invite{}, "", err
	}

	now := time.Now()
	inv := Invite{ID: ids.New("inv"), Note: "move an existing account to a new login",
		CreatedBy: adopt, CreatedAt: now, Adopts: adopt}
	if ttl > 0 {
		expires := now.Add(ttl)
		inv.ExpiresAt = &expires
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO invites (id, code_hash, note, created_by, created_at, expires_at, adopts_user_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, signup.HashCode(code), inv.Note, string(adopt), unix(now),
		nullTime(inv.ExpiresAt), string(adopt)); err != nil {
		return Invite{}, "", err
	}
	return inv, code, nil
}

// ListInvites returns every invite ever issued, newest first.
//
// Instance-wide rather than scoped to the caller: invites decide who joins the instance, not
// who reaches a mailbox, and the handler already restricts this page to the owner.
func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, note, created_by, created_at, expires_at, redeemed_by, redeemed_at,
		       revoked_at, adopts_user_id
		FROM invites ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var (
			inv                            Invite
			created                        int64
			expires, redeemedAt, revokedAt sql.NullInt64
			createdBy                      string
			redeemedBy, adopts             sql.NullString
		)
		if err := rows.Scan(&inv.ID, &inv.Note, &createdBy, &created,
			&expires, &redeemedBy, &redeemedAt, &revokedAt, &adopts); err != nil {
			return nil, err
		}
		if adopts.Valid {
			inv.Adopts = user.ID(adopts.String)
		}
		inv.CreatedBy = user.ID(createdBy)
		inv.CreatedAt = fromUnix(created)
		inv.ExpiresAt, inv.RedeemedAt, inv.RevokedAt = timePtr(expires), timePtr(redeemedAt), timePtr(revokedAt)
		if redeemedBy.Valid {
			inv.RedeemedBy = user.ID(redeemedBy.String)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite withdraws an unredeemed invite. Revoking one already redeemed does nothing:
// the account it created exists, and pretending otherwise would be misleading.
func (s *Store) RevokeInvite(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invites SET revoked_at = ? WHERE id = ? AND redeemed_at IS NULL AND revoked_at IS NULL`,
		unix(time.Now()), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNoInvite
	}
	return nil
}

// redeemInvite claims a code for a newly created user, inside the caller's transaction.
//
// The UPDATE is the check: matching on the same conditions that make an invite usable means
// two simultaneous redemptions of one code cannot both succeed, whereas a SELECT followed by
// an UPDATE would leave exactly that window open.
func redeemInvite(ctx context.Context, tx *sql.Tx, code string, by user.ID, now time.Time) (int64, error) {
	if code == "" {
		return 0, ErrNoInvite
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE invites SET redeemed_by = ?, redeemed_at = ?
		WHERE code_hash = ?
		  AND redeemed_at IS NULL
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)`,
		string(by), unix(now), signup.HashCode(code), unix(now))
	if err != nil {
		return 0, fmt.Errorf("redeeming invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNoInvite
	}
	return n, nil
}

// adoptionTarget reports which user a usable code would move, or empty for an ordinary
// invite or no match at all. It reads without claiming, so the caller can decide.
func adoptionTarget(ctx context.Context, tx *sql.Tx, code string, now time.Time) (user.ID, error) {
	var target sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT adopts_user_id FROM invites
		WHERE code_hash = ?
		  AND redeemed_at IS NULL
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)`,
		signup.HashCode(code), unix(now)).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !target.Valid {
		return "", nil
	}
	return user.ID(target.String), nil
}
