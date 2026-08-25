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

// ErrNoUser is returned when an operation needs a user that does not exist.
var ErrNoUser = errors.New("user not found")

// ErrSignupRefused is returned when an authenticated identity has no user row and the
// instance's policy will not create one.
//
// One error covering every reason it was refused. Saying "this instance is not accepting
// new accounts" rather than "no such account" matters: the second answer tells a stranger
// whether a particular person has an account here, which is the same membership leak as
// distinguishing a wrong password from an unknown user.
var ErrSignupRefused = errors.New("this instance is not accepting new accounts")

// Admission is the instance's answer to who may sign up, consulted only when an identity
// has no user row yet. An identity that already has one always signs in.
type Admission struct {
	Policy signup.Policy
	// InviteCode is whatever the person arrived holding, empty when they hold nothing.
	InviteCode string
}

// migrate brings a database created by an older version up to date.
//
// schema.sql only runs CREATE TABLE IF NOT EXISTS, which does nothing to a table that
// already exists — so a column added after that table shipped has to be added explicitly
// or an upgraded install fails at the first query with "no such column".
//
// Every column here is added nullable and nothing backfills any of them, which is the whole
// point for the four audit detail columns: an existing row keeps NULL, and the page reads
// that as "recorded before this was recorded" rather than inventing a value for a call
// nobody ever observed.
func migrate(db *sql.DB) error {
	for _, c := range []struct{ table, column, decl string }{
		{"accounts", "owner_id", "TEXT REFERENCES users(id)"},
		{"grants", "owner_id", "TEXT REFERENCES users(id)"},
		{"audit_log", "owner_id", "TEXT"},
		{"audit_log", "capability", "TEXT"},
		{"audit_log", "reason", "TEXT"},
		{"audit_log", "affected", "INTEGER"},
		{"audit_log", "detail", "TEXT"},
		{"invites", "adopts_user_id", "TEXT"},
		// Empty on every grant that predates modes, which is read as the default rather than
		// backfilled: writing `confirm` into those rows would claim somebody chose it.
		{"grants", "mode", "TEXT NOT NULL DEFAULT ''"},
		{"grants", "deleted_at", "INTEGER"},
	} {
		has, err := hasColumn(db, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.decl)); err != nil {
			return fmt.Errorf("adding %s.%s: %w", c.table, c.column, err)
		}
	}

	// An alias used to be reserved forever, so unlinking a mailbox took its name out of
	// circulation permanently and re-linking under the same name was refused. Freeing it means
	// undoing that in two places, because the old schema said it twice.
	if err := freeAliasesOfUnlinkedMailboxes(db); err != nil {
		return err
	}

	// Indexes come after the columns, which is why they are not in schema.sql: that file is
	// applied first and would otherwise try to index a column an upgraded database does not
	// have yet, failing startup for exactly the installs the migration exists to rescue.
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS accounts_owner ON accounts(owner_id)`,
		`CREATE INDEX IF NOT EXISTS grants_owner ON grants(owner_id)`,
		`CREATE INDEX IF NOT EXISTS audit_owner ON audit_log(owner_id, at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS accounts_alias_live ON accounts(alias) WHERE deleted_at IS NULL`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("creating ownership index: %w", err)
		}
	}

	return nil
}

// freeAliasesOfUnlinkedMailboxes releases an alias once its mailbox is unlinked.
//
// The old schema forbade reuse twice over: an explicit accounts_alias_ever index, and UNIQUE
// on the column itself. The first is a DROP. The second is not — SQLite builds an implicit
// index for a column constraint and gives no way to remove one, so the table has to be rebuilt
// without it. That is why this is more than a line.
//
// The rebuild is skipped entirely once it has been done, and skipped on a database created
// fresh from the current schema.sql, which never had the constraint. So the expensive path
// runs at most once per install, and never on a new one.
func freeAliasesOfUnlinkedMailboxes(db *sql.DB) error {
	if _, err := db.Exec(`DROP INDEX IF EXISTS accounts_alias_ever`); err != nil {
		return fmt.Errorf("dropping the never-reuse index: %w", err)
	}

	unique, err := hasUniqueConstraintOnAlias(db)
	if err != nil {
		return err
	}
	if !unique {
		return nil
	}

	// Every column is named rather than using SELECT *, so a mismatch between this and the
	// live table fails here instead of silently shifting a credential into another column.
	const columns = `id, owner_id, alias, address, provider, status, credential, scopes,
		linked_at, synced_at, deleted_at`

	// SQLite's own procedure for rebuilding a table requires foreign keys to be off around it:
	// the DROP and RENAME below would otherwise be seen as breaking and then re-satisfying
	// every reference into this table. Nothing references accounts today, which makes this
	// cheap insurance rather than the load-bearing part — but the next table to reference it
	// should not turn this migration into a data loss.
	//
	// It has to be set outside the transaction, because the pragma is a no-op inside one.
	if _, err := db.Exec(`PRAGMA foreign_keys = off`); err != nil {
		return fmt.Errorf("freeing unlinked aliases: %w", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = on`)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		`CREATE TABLE accounts_rebuilt (
			id          TEXT PRIMARY KEY,
			owner_id    TEXT REFERENCES users(id),
			alias       TEXT NOT NULL,
			address     TEXT NOT NULL,
			provider    TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'linked',
			credential  TEXT NOT NULL,
			scopes      TEXT NOT NULL DEFAULT '',
			linked_at   INTEGER NOT NULL,
			synced_at   INTEGER NOT NULL DEFAULT 0,
			deleted_at  INTEGER
		)`,
		`INSERT INTO accounts_rebuilt (` + columns + `) SELECT ` + columns + ` FROM accounts`,
		`DROP TABLE accounts`,
		`ALTER TABLE accounts_rebuilt RENAME TO accounts`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("freeing unlinked aliases: %w", err)
		}
	}

	// Counted inside the transaction, so a rebuild that lost a row is rolled back rather than
	// committed and discovered later by somebody whose mailbox is missing.
	var before, after int
	if err := tx.QueryRow(`SELECT count(*) FROM accounts`).Scan(&after); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&before); err == nil && before != after {
		return fmt.Errorf("freeing unlinked aliases: %d accounts before the rebuild, %d after", before, after)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Asked after the commit, because it reports on the database rather than on the
	// transaction: a rebuild that left a dangling reference is worth failing startup over
	// rather than discovering later.
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking references after the rebuild: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("freeing unlinked aliases left a dangling reference; the rebuild has " +
			"been committed and the database needs looking at by hand")
	}
	return rows.Err()
}

// hasUniqueConstraintOnAlias reports whether the table still carries UNIQUE on the column,
// which SQLite exposes as an index with origin "u" rather than as anything readable on the
// column itself.
func hasUniqueConstraintOnAlias(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA index_list(accounts)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	type index struct {
		name   string
		unique bool
		origin string
	}
	var found []index
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if unique == 1 && origin == "u" {
			found = append(found, index{name: name, unique: true, origin: origin})
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	for _, idx := range found {
		cols, err := indexColumns(db, idx.name)
		if err != nil {
			return false, err
		}
		if len(cols) == 1 && cols[0] == "alias" {
			return true, nil
		}
	}
	return false, nil
}

func indexColumns(db *sql.DB, index string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA index_info(%q)", index))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var seq, cid int
		var name sql.NullString
		if err := rows.Scan(&seq, &cid, &name); err != nil {
			return nil, err
		}
		if name.Valid {
			cols = append(cols, name.String)
		}
	}
	return cols, rows.Err()
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid                 int
			name, colType       string
			notNull, primaryKey int
			defaultValue        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// EnsureUser resolves the signed-in identity to a user row, creating it on first sight if
// the instance admits new users.
//
// Admission is evaluated inside the transaction that creates the row, so a policy check and
// the insert it authorises cannot be separated — which is what makes an invite single-use
// under two simultaneous redemptions.
//
// The second return value reports whether this call adopted pre-existing unowned data. That
// happens at most once, for the very first user ever to sign in, and it exists so that an
// instance upgraded from the single-user version does not come back with its mailboxes
// orphaned and unreachable. A second user signing in adopts nothing.
func (s *Store) EnsureUser(ctx context.Context, u user.User, adm Admission) (user.User, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return user.User{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()

	var (
		found         user.User
		created, seen int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, issuer, subject, email, name, created_at, last_seen_at
		FROM users WHERE issuer = ? AND subject = ?`, u.Issuer, u.Subject).
		Scan(&found.ID, &found.Issuer, &found.Subject, &found.Email, &found.Name, &created, &seen)

	switch {
	case err == nil:
		// Refresh the profile: an email or display name can change at the issuer, and the
		// stored copy is only ever for display.
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET email = ?, name = ?, last_seen_at = ? WHERE id = ?`,
			u.Email, u.Name, unix(now), string(found.ID)); err != nil {
			return user.User{}, false, err
		}
		found.Email, found.Name = u.Email, u.Name
		found.CreatedAt, found.LastSeenAt = fromUnix(created), now
		return found, false, tx.Commit()

	case !errors.Is(err, sql.ErrNoRows):
		return user.User{}, false, err
	}

	// An adoption invite moves an existing account onto this login instead of creating a
	// new one, so it is answered before the signup policy: it adds nobody, and it is the
	// route back in for an instance whose original login method no longer exists.
	if adm.InviteCode != "" {
		target, err := adoptionTarget(ctx, tx, adm.InviteCode, now)
		if err != nil {
			return user.User{}, false, err
		}
		if target != "" {
			adopted, err := reidentify(ctx, tx, target, u, now)
			if err != nil {
				return user.User{}, false, err
			}
			if _, err := redeemInvite(ctx, tx, adm.InviteCode, target, now); err != nil {
				return user.User{}, false, err
			}
			return adopted, false, tx.Commit()
		}
	}

	// First sight of this identity. Whether it also adopts unowned data depends on being the
	// first user overall, checked inside the same transaction so two simultaneous first
	// logins cannot both claim it.
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
		return user.User{}, false, err
	}

	// The first sign-in always succeeds whatever the policy says. Somebody has to be able to
	// use a freshly deployed instance, and the alternative is an installation step that
	// cannot be performed through the product.
	if existing > 0 {
		if err := admit(ctx, tx, adm, u, now); err != nil {
			return user.User{}, false, err
		}
	}

	fresh := user.User{
		ID: user.ID(ids.New("user")), Issuer: u.Issuer, Subject: u.Subject,
		Email: u.Email, Name: u.Name, CreatedAt: now, LastSeenAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, issuer, subject, email, name, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(fresh.ID), fresh.Issuer, fresh.Subject, fresh.Email, fresh.Name,
		unix(now), unix(now)); err != nil {
		return user.User{}, false, err
	}

	// Redeemed after the insert so the invite records the user it actually created.
	if existing > 0 && adm.Policy.Mode == signup.Invite {
		if _, err := redeemInvite(ctx, tx, adm.InviteCode, fresh.ID, now); err != nil {
			return user.User{}, false, err
		}
	}

	adopted := false
	if existing == 0 {
		accounts, err := adoptUnowned(ctx, tx, "accounts", fresh.ID)
		if err != nil {
			return user.User{}, false, err
		}
		grants, err := adoptUnowned(ctx, tx, "grants", fresh.ID)
		if err != nil {
			return user.User{}, false, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET owner_id = ? WHERE owner_id IS NULL`, string(fresh.ID)); err != nil {
			return user.User{}, false, err
		}
		adopted = accounts+grants > 0
	}

	return fresh, adopted, tx.Commit()
}

// admit applies the signup policy to an identity that has no user row.
//
// The invite branch checks only that a usable code exists; it is redeemed after the user row
// is inserted, in the same transaction, so a failure anywhere in between leaves neither the
// account nor the spent invite behind.
func admit(ctx context.Context, tx *sql.Tx, adm Admission, u user.User, now time.Time) error {
	switch adm.Policy.Mode {
	case signup.Open:
		return nil

	case signup.Allowlist:
		if adm.Policy.AllowsEmail(u.Email) {
			return nil
		}
		return ErrSignupRefused

	case signup.Invite:
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM invites
			WHERE code_hash = ?
			  AND redeemed_at IS NULL
			  AND revoked_at IS NULL
			  AND (expires_at IS NULL OR expires_at > ?)`,
			signup.HashCode(adm.InviteCode), unix(now)).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrSignupRefused
		}
		return nil

	default:
		return ErrSignupRefused
	}
}

// reidentify points an existing user row at a new login.
//
// The row keeps its id, so everything already keyed to it — mailboxes, grants, audit rows —
// follows without being touched, and its position as the earliest user is unchanged. The old
// identity simply ceases to exist, which is the honest outcome when the provider that issued
// it has been removed.
func reidentify(ctx context.Context, tx *sql.Tx, id user.ID, to user.User, now time.Time) (user.User, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE users SET issuer = ?, subject = ?, email = ?, name = ?, last_seen_at = ?
		WHERE id = ?`,
		to.Issuer, to.Subject, to.Email, to.Name, unix(now), string(id))
	if err != nil {
		return user.User{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return user.User{}, err
	} else if n == 0 {
		return user.User{}, ErrNoUser
	}

	var created int64
	var out user.User
	if err := tx.QueryRowContext(ctx, `
		SELECT id, issuer, subject, email, name, created_at FROM users WHERE id = ?`,
		string(id)).Scan(&out.ID, &out.Issuer, &out.Subject, &out.Email, &out.Name, &created); err != nil {
		return user.User{}, err
	}
	out.CreatedAt, out.LastSeenAt = fromUnix(created), now
	return out, nil
}

func adoptUnowned(ctx context.Context, tx *sql.Tx, table string, owner user.ID) (int64, error) {
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET owner_id = ? WHERE owner_id IS NULL`, table), string(owner))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// User looks up a user by id.
func (s *Store) User(ctx context.Context, id user.ID) (user.User, error) {
	var u user.User
	var created, seen int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, issuer, subject, email, name, created_at, last_seen_at
		FROM users WHERE id = ?`, string(id)).
		Scan(&u.ID, &u.Issuer, &u.Subject, &u.Email, &u.Name, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return user.User{}, ErrNoUser
	}
	if err != nil {
		return user.User{}, err
	}
	u.CreatedAt, u.LastSeenAt = fromUnix(created), fromUnix(seen)
	return u, nil
}

// CountUsers reports how many users the instance has, for the first-run experience.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ListUsers returns every user, oldest first. Used by administrative tooling rather than by
// the request path, which always knows exactly whose data it is looking at.
func (s *Store) ListUsers(ctx context.Context) ([]user.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, issuer, subject, email, name, created_at, last_seen_at
		FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []user.User
	for rows.Next() {
		var u user.User
		var created, seen int64
		if err := rows.Scan(&u.ID, &u.Issuer, &u.Subject, &u.Email, &u.Name, &created, &seen); err != nil {
			return nil, err
		}
		u.CreatedAt, u.LastSeenAt = fromUnix(created), fromUnix(seen)
		out = append(out, u)
	}
	return out, rows.Err()
}

// IsOwner reports whether a user is the one who claimed this instance.
//
// Ownership is derived from being the earliest user rather than stored on a column, because
// there is exactly one thing it currently decides — who may issue invites — and a role
// system with one role and no way to assign it would be a worse answer than a rule that
// cannot drift out of sync with reality. It also matches what already happens on first run:
// the first sign-in adopts the instance's unowned data.
func (s *Store) IsOwner(ctx context.Context, id user.ID) (bool, error) {
	// Ordered by rowid, which is insertion order, rather than by created_at: timestamps are
	// stored to the second, so two sign-ins moments apart would tie and the tie-break would
	// decide who owns the instance. Insertion order is exactly the question being asked.
	var first string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM users ORDER BY rowid LIMIT 1`).Scan(&first)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return first == string(id), nil
}
