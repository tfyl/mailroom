package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

// Retention on the held queue, against the real SQLite the server runs on.
//
// The queue is the one table in this database that stores a message body and attachment
// bytes, so what these assert is not that a row changed shape but that the mail is gone and
// that nothing can reach it afterwards.

// hold queues one action, backdated, so a test can put a row on the wrong side of a cutoff
// without waiting for a clock.
func hold(t *testing.T, s *Store, owner user.User, acct mail.Account, g grant.ID, id string, age time.Duration) held.Action {
	t.Helper()
	a := held.Action{
		ID: id, OwnerID: owner.ID, GrantID: g, AccountID: acct.ID,
		Tool: "mail_send", Kind: held.KindSend, Summary: "send the invoice to ada@example.com",
		Payload:   []byte(`{"outgoing":{"subject":"the invoice","body":{"text":"secret body"}}}`),
		CreatedAt: time.Now().Add(-age),
	}
	if err := s.HoldAction(context.Background(), owner.ID, a); err != nil {
		t.Fatalf("holding %s: %v", id, err)
	}
	return a
}

func payloadOf(t *testing.T, s *Store, id string) (payload, resolution string, resolved bool) {
	t.Helper()
	var at *int64
	if err := s.db.QueryRow(
		`SELECT payload, resolution, resolved_at FROM held_actions WHERE id = ?`, id).
		Scan(&payload, &resolution, &at); err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return payload, resolution, at != nil
}

func TestAnUnansweredHeldActionExpires(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ada := signIn(t, s, "https://idp.example.com", "ada")
	acct := link(t, s, ada, "acct_1", "work")

	stale := hold(t, s, ada, acct, "g1", "held_stale", 96*time.Hour)
	fresh := hold(t, s, ada, acct, "g1", "held_fresh", time.Hour)
	cutoff := time.Now().Add(-72 * time.Hour)

	t.Run("the sweep reclaims only what is past the cutoff", func(t *testing.T) {
		n, err := s.ExpireActions(ctx, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("expired %d actions, want 1", n)
		}
	})

	t.Run("the message is gone and the record of the request is not", func(t *testing.T) {
		payload, resolution, resolved := payloadOf(t, s, stale.ID)
		if payload != "" {
			t.Errorf("an expired action still holds its message: %q", payload)
		}
		if resolution != held.Expired {
			t.Errorf("resolution is %q, want %q", resolution, held.Expired)
		}
		if !resolved {
			t.Error("an expired action must carry a resolved_at, or it is still answerable")
		}
		var summary string
		if err := s.db.QueryRow(
			`SELECT summary FROM held_actions WHERE id = ?`, stale.ID).Scan(&summary); err != nil {
			t.Fatal(err)
		}
		if summary == "" {
			t.Error("the stub lost the line naming what was asked for, which is the half worth keeping")
		}
	})

	t.Run("a fresh action is untouched", func(t *testing.T) {
		payload, resolution, resolved := payloadOf(t, s, fresh.ID)
		if payload == "" || resolution != held.Pending || resolved {
			t.Fatalf("the sweep touched an action inside its TTL: payload=%q resolution=%q resolved=%v",
				payload, resolution, resolved)
		}
	})

	t.Run("an expired action cannot be approved", func(t *testing.T) {
		if _, err := s.ClaimAction(ctx, ada.ID, stale.ID, held.Sent, cutoff); !errors.Is(err, held.ErrNotPending) {
			t.Fatalf("claiming an expired action gave %v, want ErrNotPending", err)
		}
	})

	t.Run("an expired action cannot be discarded either", func(t *testing.T) {
		if _, err := s.ClaimAction(ctx, ada.ID, stale.ID, held.Declined, cutoff); !errors.Is(err, held.ErrNotPending) {
			t.Fatalf("discarding an expired action gave %v, want ErrNotPending", err)
		}
	})

	t.Run("the fresh one still is", func(t *testing.T) {
		got, err := s.ClaimAction(ctx, ada.ID, fresh.ID, held.Sent, cutoff)
		if err != nil {
			t.Fatalf("claiming an action inside its TTL: %v", err)
		}
		if len(got.Payload) == 0 {
			t.Error("a claim inside the TTL must hand back the payload")
		}
	})
}

// The cutoff has to be in the write, not only in the read before it. A row that reaches the
// UPDATE some other way — a race with the sweeper, a caller that skipped the SELECT — must
// still be refused, because that statement is the only thing standing between an expired
// action and the mailbox.
func TestAnExpiredActionIsRefusedByTheClaimItself(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ada := signIn(t, s, "https://idp.example.com", "ada")
	acct := link(t, s, ada, "acct_1", "work")

	a := hold(t, s, ada, acct, "g1", "held_1", 96*time.Hour)

	// Not swept. The row is still unanswered and still holds its message, so nothing but the
	// cutoff can refuse it.
	if payload, _, resolved := payloadOf(t, s, a.ID); payload == "" || resolved {
		t.Fatal("this test needs an unswept row")
	}
	if _, err := s.ClaimAction(ctx, ada.ID, a.ID, held.Sent, time.Now().Add(-72*time.Hour)); !errors.Is(err, held.ErrNotPending) {
		t.Fatalf("claiming past the cutoff gave %v, want ErrNotPending", err)
	}
}

// A zero cutoff is how an instance with retention turned off asks for the behaviour this
// table used to have. It must expire nothing rather than everything: the two mistakes are
// one sign apart and only one of them destroys mail.
func TestRetentionOffExpiresNothing(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ada := signIn(t, s, "https://idp.example.com", "ada")
	acct := link(t, s, ada, "acct_1", "work")

	ancient := hold(t, s, ada, acct, "g1", "held_old", 365*24*time.Hour)

	n, err := s.ExpireActions(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a zero cutoff expired %d actions, want 0", n)
	}

	pending, err := s.PendingActions(ctx, ada.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("a zero cutoff left %d actions waiting, want 1", len(pending))
	}
	if _, err := s.ClaimAction(ctx, ada.ID, ancient.ID, held.Sent, time.Time{}); err != nil {
		t.Fatalf("with retention off a year-old action is still answerable: %v", err)
	}
}

// An expired action is not waiting for anybody, so it must not be listed as pending, and it
// must not spend a slot in the per-grant cap on unanswered actions. A grant whose queue is
// fifty abandoned rows would otherwise never be able to queue anything again.
func TestExpiredActionsAreNeitherListedNorCounted(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ada := signIn(t, s, "https://idp.example.com", "ada")
	acct := link(t, s, ada, "acct_1", "work")

	hold(t, s, ada, acct, "g1", "held_stale", 96*time.Hour)
	hold(t, s, ada, acct, "g1", "held_fresh", time.Hour)
	cutoff := time.Now().Add(-72 * time.Hour)

	// Before any sweep: the cutoff alone has to be enough, or a stale row is served in the
	// window between one sweep and the next.
	pending, err := s.PendingActions(ctx, ada.ID, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "held_fresh" {
		t.Fatalf("pending is %v, want only held_fresh", actionIDs(pending))
	}

	n, err := s.CountPending(ctx, ada.ID, "g1", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the cap counted %d waiting actions, want 1", n)
	}
}

// An expired row belongs in the closed list, where the page can say what became of it.
func TestAnExpiredActionShowsInTheHistory(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ada := signIn(t, s, "https://idp.example.com", "ada")
	acct := link(t, s, ada, "acct_1", "work")

	hold(t, s, ada, acct, "g1", "held_stale", 96*time.Hour)
	if _, err := s.ExpireActions(ctx, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	recent, err := s.RecentActions(ctx, ada.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Resolution != held.Expired {
		t.Fatalf("history is %v, want one expired action", recent)
	}
	if len(recent[0].Payload) != 0 {
		t.Error("the history is drawn from the same row; it must not carry the message")
	}
}

func actionIDs(in []held.Action) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.ID)
	}
	return out
}
