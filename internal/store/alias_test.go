package store

import (
	"context"
	"errors"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

func TestRenamingAMailbox(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	ada := signIn(t, s, "https://idp.example.com", "ada")
	box := link(t, s, ada, "acct_one", "work")

	if err := s.RenameAccount(ctx, ada.ID, box.ID, "personal"); err != nil {
		t.Fatalf("renaming: %v", err)
	}

	t.Run("the new name resolves", func(t *testing.T) {
		got, err := s.AccountByAlias(ctx, ada.ID, "personal")
		if err != nil || got.ID != box.ID {
			t.Fatalf("got %+v, %v; want the renamed mailbox", got, err)
		}
	})

	t.Run("the old name resolves to nothing", func(t *testing.T) {
		if _, err := s.AccountByAlias(ctx, ada.ID, "work"); !errors.Is(err, mail.ErrNotFound) {
			t.Fatalf("want ErrNotFound for the old alias, got %v", err)
		}
	})

	// Renaming releases the name it had, as unlinking now does too. A caller still holding the
	// old one finds a different mailbox rather than nothing, which is why tool results carry
	// the address beside the alias.
	t.Run("and the old name is free for another mailbox", func(t *testing.T) {
		other := mail.Account{
			ID: "acct_two", Alias: "work", Address: "other@example.com",
			Provider: mail.ProviderGmail, Status: mail.StatusLinked,
		}
		if err := s.LinkAccount(ctx, ada.ID, other, "sealed", "scopes"); err != nil {
			t.Fatalf("linking under the released alias: %v", err)
		}
	})
}

// A name belongs to one live mailbox at a time, and unlinking gives it back.
//
// This asserted the opposite until aliases became reusable: an unlinked mailbox used to keep
// its name forever, which meant re-linking a mailbox under the name it already had was
// refused. That is the ordinary case after a provider's scopes change, so the reservation
// cost more than it protected — grants store immutable ids and cannot inherit a reused name.
func TestAnAliasBelongsToOneLiveMailbox(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	ada := signIn(t, s, "https://idp.example.com", "ada")
	box := link(t, s, ada, "acct_one", "work")
	live := link(t, s, ada, "acct_two", "personal")
	gone := link(t, s, ada, "acct_three", "archive")
	if err := s.UnlinkAccount(ctx, ada.ID, gone.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameAccount(ctx, ada.ID, box.ID, live.Alias); err == nil {
		t.Fatalf("renaming into %q should have been refused: another mailbox is using it", live.Alias)
	}
	if err := s.RenameAccount(ctx, ada.ID, box.ID, gone.Alias); err != nil {
		t.Fatalf("renaming into the freed name %q: %v", gone.Alias, err)
	}
	if err := s.RenameAccount(ctx, ada.ID, box.ID, "work"); err != nil {
		t.Fatalf("renaming back: %v", err)
	}

	got, err := s.Account(ctx, ada.ID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "work" {
		t.Fatalf("a refused rename changed the alias to %q", got.Alias)
	}
}

func TestRenamingIsScopedToTheOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	ada := signIn(t, s, "https://idp.example.com", "ada")
	bob := signIn(t, s, "https://idp.example.com", "bob")
	adaBox := link(t, s, ada, "acct_ada", "ada-work")

	if err := s.RenameAccount(ctx, bob.ID, adaBox.ID, "bob-work"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("want ErrNotFound renaming somebody else's mailbox, got %v", err)
	}

	got, err := s.Account(ctx, ada.ID, adaBox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "ada-work" {
		t.Fatalf("alias is now %q", got.Alias)
	}
}

// Renaming an unlinked mailbox relabels a row nothing can reach, and releases the alias that
// row was holding as a side effect.
func TestRenamingAnUnlinkedMailboxIsRefused(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	ada := signIn(t, s, "https://idp.example.com", "ada")
	box := link(t, s, ada, "acct_one", "work")

	if err := s.UnlinkAccount(ctx, ada.ID, box.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameAccount(ctx, ada.ID, box.ID, "revived"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
