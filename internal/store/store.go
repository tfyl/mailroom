// Package store persists accounts, grants, clients and the audit log.
//
// SQLite by default because the whole product should run from `docker run` with no external
// services. The interface is narrow enough that a Postgres implementation is a drop-in for
// anyone running more than one replica.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
}

// Open connects to the database named by a URL and applies the schema.
func Open(dsn string) (*Store, error) {
	path, ok := strings.CutPrefix(dsn, "sqlite://")
	if !ok {
		return nil, fmt.Errorf("only sqlite:// URLs are supported in this build, got %q", dsn)
	}

	// WAL keeps readers from blocking the writer, and a busy timeout turns the occasional
	// concurrent write into a short wait rather than an immediate "database is locked".
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

// --- Accounts ---
//
// Every lookup takes the owner explicitly. Passing it rather than reading it from a context
// is deliberate: an unscoped query is then a compile error instead of a silent read of
// somebody else's mailbox.

// Account is looked up by immutable id, scoped to its owner. This is the path grants use.
func (s *Store) Account(ctx context.Context, owner user.ID, id mail.AccountID) (mail.Account, error) {
	return s.account(ctx, "id = ? AND owner_id = ? AND deleted_at IS NULL", string(id), string(owner))
}

func (s *Store) AccountByAlias(ctx context.Context, owner user.ID, alias string) (mail.Account, error) {
	return s.account(ctx, "alias = ? AND owner_id = ? AND deleted_at IS NULL", alias, string(owner))
}

func (s *Store) AccountByAddress(ctx context.Context, owner user.ID, address string) (mail.Account, error) {
	return s.account(ctx, "address = ? AND owner_id = ? AND deleted_at IS NULL", address, string(owner))
}

func (s *Store) account(ctx context.Context, where string, args ...any) (mail.Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(owner_id, ''), alias, address, provider, status, linked_at
		FROM accounts WHERE `+where, args...)

	var a mail.Account
	var linked int64
	err := row.Scan(&a.ID, &a.OwnerID, &a.Alias, &a.Address, &a.Provider, &a.Status, &linked)
	if errors.Is(err, sql.ErrNoRows) {
		// A mailbox owned by somebody else is reported as missing rather than forbidden.
		// Confirming that an id exists but belongs to another user is itself a disclosure.
		return mail.Account{}, mail.ErrNotFound
	}
	if err != nil {
		return mail.Account{}, err
	}
	a.LinkedAt = fromUnix(linked)
	return a, nil
}

func (s *Store) ListAccounts(ctx context.Context, owner user.ID) ([]mail.Account, error) {
	// Last use is derived from the audit log rather than stored on the account. The audit
	// log already records every tool call against every mailbox, so a synced_at column meant
	// writing the same fact twice — once per call, on a server whose job is proxying rather
	// than syncing. It was written nowhere and read as zero, so the page never showed it.
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, COALESCE(a.owner_id, ''), a.alias, a.address, a.provider, a.status,
		       a.linked_at, COALESCE(MAX(l.at), 0)
		FROM accounts a
		LEFT JOIN audit_log l ON l.account_id = a.id AND l.owner_id = a.owner_id
		WHERE a.owner_id = ? AND a.deleted_at IS NULL
		GROUP BY a.id
		ORDER BY a.alias`, string(owner))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []mail.Account
	for rows.Next() {
		var a mail.Account
		var linked, used int64
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Alias, &a.Address, &a.Provider, &a.Status, &linked, &used); err != nil {
			return nil, err
		}
		a.LinkedAt, a.LastUsedAt = fromUnix(linked), fromUnix(used)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LinkAccount stores a newly linked mailbox with its sealed credential.
//
// The alias is unique across the whole instance, not per user. Two people cannot both call a
// mailbox "work": aliases appear in every tool call and in grant records, and one meaning
// two mailboxes depending on who is asking is a confusion worth avoiding entirely.
//
// Unique among *live* mailboxes. An unlinked one releases its name, so re-linking under the
// name it already had works — which is the ordinary case after a provider's scopes change or
// a token is revoked, and used to be refused outright.
func (s *Store) LinkAccount(ctx context.Context, owner user.ID, a mail.Account, sealedCredential, scopes string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, owner_id, alias, address, provider, status, credential, scopes, linked_at, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		string(a.ID), string(owner), a.Alias, a.Address, string(a.Provider), string(mail.StatusLinked),
		sealedCredential, scopes, unix(time.Now()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("the alias %q already names another linked mailbox; unlink that one "+
			"or choose a different name", a.Alias)
	}
	return err
}

// Credential returns the sealed credential for an account. Callers unseal it with the
// account id as context.
func (s *Store) Credential(ctx context.Context, owner user.ID, id mail.AccountID) (string, error) {
	var sealed string
	err := s.db.QueryRowContext(ctx,
		`SELECT credential FROM accounts WHERE id = ? AND owner_id = ? AND deleted_at IS NULL`,
		string(id), string(owner)).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", mail.ErrNotFound
	}
	return sealed, err
}

func (s *Store) UpdateCredential(ctx context.Context, owner user.ID, id mail.AccountID, sealed string) error {
	return s.affectOne(ctx,
		`UPDATE accounts SET credential = ?, status = 'linked' WHERE id = ? AND owner_id = ?`,
		sealed, string(id), string(owner))
}

func (s *Store) SetAccountStatus(ctx context.Context, owner user.ID, id mail.AccountID, status mail.AccountStatus) error {
	return s.affectOne(ctx,
		`UPDATE accounts SET status = ? WHERE id = ? AND owner_id = ?`,
		string(status), string(id), string(owner))
}

// RenameAccount changes the alias. Grants are unaffected because they store ids.
//
// The old name is released, as it is by unlinking. A stale reference to a freed name can
// therefore come to mean something else, which is why tool results name the address alongside
// the alias rather than the alias alone.
func (s *Store) RenameAccount(ctx context.Context, owner user.ID, id mail.AccountID, alias string) error {
	// deleted_at is checked here but not in the sibling updates: renaming an unlinked mailbox
	// relabels a row nothing can reach, and frees that row's alias as a side effect.
	err := s.affectOne(ctx,
		`UPDATE accounts SET alias = ? WHERE id = ? AND owner_id = ? AND deleted_at IS NULL`,
		alias, string(id), string(owner))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("the alias %q is already in use", alias)
	}
	return err
}

// UnlinkAccount soft-deletes: the row survives so audit history still resolves the mailbox it
// names, and the credential is blanked so nothing can be opened with it.
//
// The alias is released. It used to stay reserved forever, which meant re-linking a mailbox
// under its own name was impossible — and what that was protecting is narrower than it looks,
// since grants store immutable ids and cannot inherit a reused name. What can follow a name is
// a grant's default_scope selector, which holds bare aliases.
func (s *Store) UnlinkAccount(ctx context.Context, owner user.ID, id mail.AccountID) error {
	return s.affectOne(ctx,
		`UPDATE accounts SET deleted_at = ?, credential = '' WHERE id = ? AND owner_id = ?`,
		unix(time.Now()), string(id), string(owner))
}

// affectOne runs a scoped update and reports ErrNotFound when it matched nothing.
//
// An owner-scoped UPDATE that hits zero rows is the shape a cross-user attempt takes, and
// reporting success for it would let the UI cheerfully claim to have unlinked a mailbox it
// never touched.
func (s *Store) affectOne(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return mail.ErrNotFound
	}
	return nil
}
