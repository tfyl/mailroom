package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/held"
)

// held_actions as it shipped, before anything reclaimed a row nobody answered.
//
// Written out rather than derived from schema.sql, because the point of the test is the gap
// between the two: an install created from this file has a populated table and none of the
// index the sweeper wants, which is the state every upgrade starts in and the state a fresh
// database never visits. The columns are the same ones — expiry needed no new column, which
// is the other half of what this asserts, since a schema.sql index over a column added by a
// migration is the bug that broke every populated install last time.
const schemaBeforeHeldRetention = `
CREATE TABLE held_actions (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL,
    grant_id    TEXT NOT NULL,
    account_id  TEXT NOT NULL,
    tool        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    summary     TEXT NOT NULL,
    payload     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    resolved_at INTEGER,
    resolution  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX held_owner ON held_actions(owner_id, created_at DESC);
CREATE INDEX held_grant ON held_actions(grant_id, resolved_at);
`

func TestUpgradingAPopulatedDatabaseReclaimsItsUnansweredActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaBeforeHeldRetention); err != nil {
		t.Fatalf("building the old schema: %v", err)
	}

	// A queue as an install that has been running for months would have it: something
	// answered, something abandoned long ago, and something queued this morning. The
	// abandoned one is the row this whole change exists for — it has been holding a message
	// since long before the server that will now expire it was built.
	now := time.Now()
	for _, row := range []struct {
		id, summary, payload, resolution string
		age                              time.Duration
		resolved                         any
	}{
		{"held_answered", "send the agenda to ada@example.com", "", held.Sent, 200 * 24 * time.Hour, now.Add(-200 * 24 * time.Hour).Unix()},
		{"held_abandoned", "send the invoice to bob@example.com", `{"outgoing":{"body":{"text":"a body nobody approved"}}}`, held.Pending, 180 * 24 * time.Hour, nil},
		{"held_today", "send the notes to cat@example.com", `{"outgoing":{"body":{"text":"queued this morning"}}}`, held.Pending, 2 * time.Hour, nil},
	} {
		if _, err := raw.Exec(`INSERT INTO held_actions
			(id, owner_id, grant_id, account_id, tool, kind, summary, payload, created_at, resolved_at, resolution)
			VALUES (?, 'user_ada', 'g1', 'acct_1', 'mail_send', 'send', ?, ?, ?, ?, ?)`,
			row.id, row.summary, row.payload, now.Add(-row.age).Unix(), row.resolved, row.resolution); err != nil {
			t.Fatalf("seeding %s: %v", row.id, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening applies schema.sql and then migrate, which is the only path a real upgrade
	// takes. It has to survive a held_actions that already exists and is not empty.
	s, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("opening the upgraded database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	t.Run("the sweeper's index exists on the old table", func(t *testing.T) {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'held_unanswered'`).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatal("the upgrade did not create held_unanswered, so every sweep scans the table")
		}
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("nothing that was already there was lost", func(t *testing.T) {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM held_actions`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Fatalf("the upgrade left %d rows, want 3", count)
		}
	})

	cutoff := now.Add(-72 * time.Hour)

	t.Run("the first sweep reclaims the abandoned message", func(t *testing.T) {
		n, err := s.ExpireActions(ctx, cutoff)
		if err != nil {
			t.Fatalf("sweeping the upgraded database: %v", err)
		}
		if n != 1 {
			t.Fatalf("expired %d actions, want 1 — only held_abandoned is past the cutoff "+
				"and unanswered", n)
		}
		payload, resolution, resolved := payloadOf(t, s, "held_abandoned")
		if payload != "" {
			t.Errorf("a message queued 180 days ago survived the upgrade: %q", payload)
		}
		if resolution != held.Expired || !resolved {
			t.Errorf("held_abandoned came back resolution=%q resolved=%v", resolution, resolved)
		}
	})

	t.Run("an action answered before the upgrade is left as it was", func(t *testing.T) {
		_, resolution, resolved := payloadOf(t, s, "held_answered")
		if resolution != held.Sent || !resolved {
			t.Fatalf("the sweep rewrote an answered row: resolution=%q resolved=%v",
				resolution, resolved)
		}
	})

	t.Run("this morning's action is still waiting and still answerable", func(t *testing.T) {
		pending, err := s.PendingActions(ctx, "user_ada", cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 || pending[0].ID != "held_today" {
			t.Fatalf("pending is %v, want only held_today", actionIDs(pending))
		}
		if _, err := s.ClaimAction(ctx, "user_ada", "held_today", held.Sent, cutoff); err != nil {
			t.Fatalf("claiming an action inside its TTL after an upgrade: %v", err)
		}
	})

	t.Run("the abandoned one cannot be approved after the upgrade", func(t *testing.T) {
		_, err := s.ClaimAction(ctx, "user_ada", "held_abandoned", held.Sent, cutoff)
		if !errors.Is(err, held.ErrNotPending) {
			t.Fatalf("claiming an expired action gave %v, want ErrNotPending", err)
		}
	})

	t.Run("opening again changes nothing", func(t *testing.T) {
		if err := migrate(s.db); err != nil {
			t.Fatalf("second migration: %v", err)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM held_actions`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Fatalf("a repeat migration left %d rows, want 3", count)
		}
	})
}
