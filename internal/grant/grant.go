package grant

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/user"
)

type ID string

// Grant is the unit of access: a named, revocable permission for one MCP client to use a
// specific set of mailboxes in a specific set of ways.
//
// Accounts holds immutable account IDs rather than aliases. If it held aliases, renaming a
// mailbox would break every grant naming it, and — much worse — deleting a mailbox and
// nicknaming a different one with the freed alias would silently hand old grants access to
// a mailbox nobody approved them for.
type Grant struct {
	ID ID
	// OwnerID is the user who approved this grant. Every account it names belongs to them,
	// which is what stops one person's client token from reaching another person's mail on
	// a shared instance.
	OwnerID  user.ID
	ClientID string
	Label    string
	Accounts []mail.AccountID
	Caps     mail.Set
	// Mode is how much this client may do on its own initiative. It is a plain field with no
	// accessor on purpose: the zero value is what every grant approved before modes existed
	// carries, and every method on Mode resolves that to the default, so an unset mode
	// behaves as `confirm` wherever it is asked rather than only where a call site
	// remembered to check.
	//
	// Nothing reachable from an MCP client writes it. Its two writers are the consent screen
	// and the grant edit page, both behind an authenticated browser session. See mode.go.
	Mode       Mode
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

var (
	ErrRevoked  = errors.New("grant has been revoked")
	ErrExpired  = errors.New("grant has expired")
	ErrNoScope  = errors.New("grant names no accounts")
	ErrNotFound = errors.New("grant not found")
)

// Valid reports whether the grant may be used at all, before any per-call checks.
func (g *Grant) Valid(now time.Time) error {
	switch {
	case g.RevokedAt != nil:
		return ErrRevoked
	case g.ExpiresAt != nil && now.After(*g.ExpiresAt):
		return ErrExpired
	case len(g.Accounts) == 0:
		return ErrNoScope
	}
	return nil
}

// HasAccount reports whether this grant names the given account.
func (g *Grant) HasAccount(id mail.AccountID) bool {
	for _, a := range g.Accounts {
		if a == id {
			return true
		}
	}
	return false
}

// Authorize is the single gate every tool call passes through. It answers one question: may
// this grant perform this capability against this account?
//
// The account check runs before the capability check so that a grant which cannot see an
// account at all never learns which capabilities that account supports.
func (g *Grant) Authorize(now time.Time, account mail.Account, c mail.Capability) error {
	if err := g.Valid(now); err != nil {
		return err
	}
	if !g.HasAccount(account.ID) {
		return &mail.ScopeError{Account: account.Alias, Address: account.Address}
	}
	if !g.Caps.Has(c) {
		return &mail.ScopeError{
			Account: account.Alias, Address: account.Address,
			Capability: c, Held: g.Caps,
		}
	}
	if account.Status == mail.StatusDisabled {
		return fmt.Errorf("account %s is disabled", account.Alias)
	}
	if account.Status == mail.StatusNeedsReauth {
		return mail.ErrNeedsReauth
	}
	return nil
}

// Expired reports whether the grant has passed its expiry, without treating a revoked grant
// as expired — the UI shows them differently.
func (g *Grant) Expired(now time.Time) bool {
	return g.ExpiresAt != nil && now.After(*g.ExpiresAt)
}

func (g *Grant) Revoked() bool { return g.RevokedAt != nil }

// Active reports whether the grant is usable right now.
func (g *Grant) Active(now time.Time) bool { return g.Valid(now) == nil }

// MaxDays is the longest expiry a grant may be given: ten years, which is past any honest
// use and short of the overflow.
const MaxDays = 3650

// ParseExpiry turns a form's choice of expiry into an absolute time, or nil for a grant that
// never expires.
//
// It lives here rather than beside either form because the consent screen and the edit page
// are two ways of setting the same field, and an edit that accepted a value consent refuses
// would be a way round the check rather than a second one. Anything unreadable is an error
// rather than quietly becoming a grant that never expires, and the upper bound is not
// pedantry: a large enough number of days overflows the duration and lands the expiry in the
// past, where it reads as an expired grant that nobody chose to expire.
func ParseExpiry(days string, now time.Time) (*time.Time, error) {
	if days == "" || days == "never" {
		return nil, nil
	}
	n, err := strconv.Atoi(days)
	if err != nil || n <= 0 || n > MaxDays {
		return nil, fmt.Errorf(
			"expires_days must be a whole number of days between 1 and %d, or \"never\"", MaxDays)
	}
	at := now.Add(time.Duration(n) * 24 * time.Hour)
	return &at, nil
}

// TouchInterval is how stale a recorded last use has to be before it is worth writing again.
//
// MCP clients poll, so a write per request would put a steady stream of them on the hot path
// for a value the page renders to the minute. Anything finer than this is written and never
// read.
const TouchInterval = time.Minute

// NeedsTouch reports whether this grant's recorded last use is stale enough to replace.
func (g *Grant) NeedsTouch(now time.Time) bool {
	return g.LastUsedAt == nil || now.Sub(*g.LastUsedAt) >= TouchInterval
}
