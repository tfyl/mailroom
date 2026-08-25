package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// The schema that shipped before aliases became reusable. It says "never reused" twice: an
// explicit index, and UNIQUE on the column, which SQLite backs with an index that cannot be
// dropped. Only a table rebuild removes the second, and this is the table holding every
// mailbox credential — so what this test is really asserting is that the rebuild does not
// lose one.
const schemaBeforeReusableAliases = `
CREATE TABLE accounts (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT REFERENCES users(id),
	alias       TEXT NOT NULL UNIQUE,
	address     TEXT NOT NULL,
	provider    TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'linked',
	credential  TEXT NOT NULL,
	scopes      TEXT NOT NULL DEFAULT '',
	linked_at   INTEGER NOT NULL,
	synced_at   INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER
);
CREATE UNIQUE INDEX accounts_alias_ever ON accounts(alias);
`

func TestMigratingFreesTheNameOfAnUnlinkedMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaBeforeReusableAliases); err != nil {
		t.Fatalf("building the old schema: %v", err)
	}
	for _, row := range []struct {
		id, alias, addr, cred string
		deleted               any
	}{
		{"acct_live", "work", "ada@work.example", "sealed-live", nil},
		{"acct_gone", "archive", "ada@old.example", "", 1700000000},
	} {
		if _, err := raw.Exec(`INSERT INTO accounts
			(id, owner_id, alias, address, provider, status, credential, scopes, linked_at, synced_at, deleted_at)
			VALUES (?, NULL, ?, ?, 'gmail', 'linked', ?, 'scopes', 1, 0, ?)`,
			row.id, row.alias, row.addr, row.cred, row.deleted); err != nil {
			t.Fatalf("seeding %s: %v", row.id, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening runs the migration.
	s, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("opening the upgraded database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	t.Run("every mailbox and its credential survives the rebuild", func(t *testing.T) {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("the rebuild left %d accounts, want 2", count)
		}
		var cred, addr string
		if err := s.db.QueryRow(
			`SELECT credential, address FROM accounts WHERE id = 'acct_live'`).Scan(&cred, &addr); err != nil {
			t.Fatal(err)
		}
		if cred != "sealed-live" || addr != "ada@work.example" {
			t.Errorf("the live mailbox came back as credential=%q address=%q", cred, addr)
		}
	})

	ada := signIn(t, s, "https://idp.example.com", "ada")

	t.Run("the unlinked mailbox's name is free", func(t *testing.T) {
		err := s.LinkAccount(ctx, ada.ID, mail.Account{
			ID: "acct_new", Alias: "archive", Address: "ada@new.example",
			Provider: mail.ProviderGmail, Status: mail.StatusLinked,
		}, "sealed-new", "scopes")
		if err != nil {
			t.Fatalf("linking under the freed name: %v", err)
		}
	})

	t.Run("a live mailbox's name is still not", func(t *testing.T) {
		err := s.LinkAccount(ctx, ada.ID, mail.Account{
			ID: "acct_clash", Alias: "work", Address: "someone@else.example",
			Provider: mail.ProviderGmail, Status: mail.StatusLinked,
		}, "sealed", "scopes")
		if err == nil {
			t.Fatal("two live mailboxes must not share a name")
		}
	})

	t.Run("migrating again changes nothing", func(t *testing.T) {
		if err := migrate(s.db); err != nil {
			t.Fatalf("second migration: %v", err)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Fatalf("a repeat migration left %d accounts, want 3", count)
		}
	})
}
