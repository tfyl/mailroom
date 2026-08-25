package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Blob metadata lives here rather than beside the bytes, which is what lets the local
// directory backend and any future object store share one set of rules. Where the content
// sits is a storage question; who may reach it, until when, and under which grant are
// database questions, and they are answered by the same queries — owner-scoped, re-read per
// request — that answer them for a mailbox.

const blobColumns = `id, owner_id, grant_id, kind, state, account_id, filename, mime_type,
	size, reserved, created_at, expires_at`

func (s *Store) PutBlob(ctx context.Context, owner user.ID, r blob.Ref) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blobs (`+blobColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, string(owner), string(r.GrantID), string(r.Kind), string(r.State),
		string(r.AccountID), r.Filename, r.MimeType, r.Size, r.Reserved,
		r.CreatedAt.Unix(), r.ExpiresAt.Unix())
	return err
}

func (s *Store) Blob(ctx context.Context, owner user.ID, id string) (blob.Ref, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+blobColumns+` FROM blobs WHERE id = ? AND owner_id = ?`, id, string(owner))
	return scanBlob(row)
}

func (s *Store) DeleteBlob(ctx context.Context, owner user.ID, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM blobs WHERE id = ? AND owner_id = ?`, id, string(owner))
	return err
}

// ClaimBlob is the single-use check on an upload URL, and it is one statement for a reason.
//
// Reading the state and then writing it would leave a window in which two concurrent PUTs
// both see `pending`, both proceed, and the second silently replaces bytes the first already
// handed a blob id for. The conditional UPDATE closes that: exactly one caller changes a row
// from pending, and every other gets nothing back.
func (s *Store) ClaimBlob(ctx context.Context, owner user.ID, id string, now time.Time) (blob.Ref, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE blobs SET state = ?
		WHERE id = ? AND owner_id = ? AND state = ? AND expires_at > ?`,
		string(blob.StateUploading), id, string(owner), string(blob.StatePending), now.Unix())
	if err != nil {
		return blob.Ref{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return blob.Ref{}, err
	}
	if n == 0 {
		// Either it was never pending, or somebody else took it. Both mean the same thing to
		// whoever is holding the URL, and separating them would report on a request that is
		// not theirs.
		ref, lookupErr := s.Blob(ctx, owner, id)
		if lookupErr != nil {
			return blob.Ref{}, lookupErr
		}
		if ref.State != blob.StatePending {
			return blob.Ref{}, blob.ErrClaimed
		}
		return blob.Ref{}, blob.ErrGone
	}
	return s.Blob(ctx, owner, id)
}

func (s *Store) CompleteBlob(ctx context.Context, owner user.ID, id string, size int64) error {
	// Reserved drops to zero as the real size takes over, so a completed upload is charged
	// what it actually weighs rather than what it was allowed to.
	return s.affectOne(ctx, `
		UPDATE blobs SET state = ?, size = ?, reserved = 0
		WHERE id = ? AND owner_id = ? AND state = ?`,
		string(blob.StateReady), size, id, string(owner), string(blob.StateUploading))
}

// ExpiredBlobs is deliberately not owner-scoped: sweeping is maintenance across the whole
// store rather than anybody's read of their own data, and a per-owner sweep would need a list
// of owners to be complete. Every row it returns carries its owner, so the deletes that
// follow are scoped like every other write.
func (s *Store) ExpiredBlobs(ctx context.Context, before time.Time) ([]blob.Ref, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+blobColumns+` FROM blobs WHERE expires_at <= ?`, before.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []blob.Ref
	for rows.Next() {
		ref, err := scanBlob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// A pending upload is charged the size it was promised rather than the nothing it currently
// holds, so that minting many upload URLs cannot walk past a quota that only counts arrived
// bytes.
const chargedBytes = `COALESCE(SUM(CASE WHEN state = 'ready' THEN size ELSE reserved END), 0)`

func (s *Store) OwnerBlobBytes(ctx context.Context, owner user.ID) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT `+chargedBytes+` FROM blobs WHERE owner_id = ?`, string(owner)).Scan(&total)
	return total, err
}

func (s *Store) TotalBlobBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT `+chargedBytes+` FROM blobs`).Scan(&total)
	return total, err
}

func scanBlob(row rowScanner) (blob.Ref, error) {
	var (
		r                 blob.Ref
		owner, grantID    string
		kind, state       string
		account           string
		created, expires  int64
		size, reservedInt int64
	)
	err := row.Scan(&r.ID, &owner, &grantID, &kind, &state, &account, &r.Filename,
		&r.MimeType, &size, &reservedInt, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return blob.Ref{}, blob.ErrNotFound
	}
	if err != nil {
		return blob.Ref{}, err
	}
	r.Owner = user.ID(owner)
	r.GrantID = grant.ID(grantID)
	r.Kind = blob.Kind(kind)
	r.State = blob.State(state)
	r.AccountID = mail.AccountID(account)
	r.Size = size
	r.Reserved = reservedInt
	r.CreatedAt = fromUnix(created)
	r.ExpiresAt = fromUnix(expires)
	return r, nil
}
