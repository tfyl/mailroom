// Package user models the people who administer a mailroom instance.
//
// A user is the owner of mailboxes and of the grants issued against them. Everything an
// instance holds belongs to exactly one, which is what lets several people share a
// deployment without sharing a mailbox.
//
// This is deliberately separate from the MCP grant model. A user is a human who signs in; a
// grant is a permission handed to a program. Conflating them is how an instance ends up
// where any client token can reach any mailbox.
package user

import (
	"context"
	"time"
)

type ID string

// User is an authenticated human.
//
// Identity is keyed on (Issuer, Subject) rather than on Subject or Email alone. Subjects are
// only unique within an issuer, so two issuers can legitimately hand out the same one — and
// email addresses change, get reassigned inside an organisation, and are not guaranteed
// unique by every provider. Keying on the pair means switching identity providers creates a
// new user rather than silently granting someone else's mail.
type User struct {
	ID         ID
	Issuer     string
	Subject    string
	Email      string
	Name       string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Display returns the friendliest identifier available for this user.
func (u User) Display() string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	default:
		return u.Subject
	}
}

type contextKey struct{}

// NewContext carries the signed-in user through a request.
//
// The context is how handlers *read* the current user; it is never how a query is scoped.
// Store calls take the owner as an explicit argument so that forgetting to scope one is a
// compile error rather than a silent leak of somebody else's mail.
func NewContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}
