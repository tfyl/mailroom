package mail

import (
	"errors"
	"fmt"
)

// The three failure kinds below must never collapse into one another. The correct response
// to each differs: a denied scope needs the operator, an unsupported operation needs a
// different account, and a provider error may simply need retrying. A model that cannot tell
// them apart will retry a permission problem forever or give up on a transient one.

// All three name the mailbox with two fields rather than one. Account is the alias, which is
// what a caller selects by and what these errors stay keyed on; Address is the mailbox that
// alias currently points at. The message text names both, because "this grant does not
// include work" is unanswerable to anyone who has to decide whether work is the mailbox they
// meant — and these messages are written to be relayed to a person.

// ScopeError means the calling grant does not permit this. The operator must widen the grant.
type ScopeError struct {
	Account    string // alias, for a human
	Address    string // the address that alias currently names; empty when nothing resolved
	Capability Capability
	Held       Set
}

func (e *ScopeError) Error() string {
	mailbox := displayMailbox(e.Account, e.Address)
	if e.Capability == "" {
		return fmt.Sprintf("scope_denied: this grant does not include the account %q", mailbox)
	}
	held := e.Held.String()
	if held == "" {
		held = "nothing"
	}
	return fmt.Sprintf(
		"scope_denied: this grant holds %s on %s. That action requires %q.",
		held, mailbox, e.Capability,
	)
}

// UnsupportedError means the provider cannot do this at all, whatever the grant says.
//
// Op and Reason exist because a capability is not always unavailable as a whole. Gmail
// implements every settings operation, and refuses exactly one of them on a consumer account
// — reporting that as "does not support settings" is wrong in a way that costs somebody an
// afternoon, since five neighbouring operations on the same mailbox work.
type UnsupportedError struct {
	Provider   ProviderID
	Account    string
	Address    string
	Capability Capability

	// Op names the single operation, when the rest of the capability is available.
	Op string
	// Reason says why, in words meant for whoever has to do something about it.
	Reason string
}

func (e *UnsupportedError) Error() string {
	what := string(e.Capability)
	if e.Op != "" {
		what = e.Op
	}
	msg := fmt.Sprintf("unsupported_by_provider: %s (%s) does not support %q",
		displayMailbox(e.Account, e.Address), e.Provider, what)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// ProviderError wraps a failure from the upstream mail service. Retryable distinguishes a
// throttle or a blip from a permanent rejection.
type ProviderError struct {
	Provider  ProviderID
	Account   string
	Address   string
	Op        string
	Retryable bool
	RetryIn   int // seconds; 0 when unknown
	Err       error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider_error: %s on %s (%s): %v",
		e.Op, displayMailbox(e.Account, e.Address), e.Provider, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// ErrNeedsReauth is returned when stored credentials no longer work. It is separated from a
// generic provider error because the fix is an operator visiting the UI, not a retry.
var ErrNeedsReauth = errors.New("account credentials expired; re-link required")

var ErrNotFound = errors.New("not found")

// CodeAuthExpired is named rather than spelled out because more than one package has to
// recognise it: a client matches on it, and the server marks the mailbox when it sees it.
const CodeAuthExpired = "auth_expired"

// Code maps an error to the stable string clients match on.
func Code(err error) string {
	var scope *ScopeError
	var unsup *UnsupportedError
	var prov *ProviderError
	switch {
	case errors.As(err, &scope):
		return "scope_denied"
	case errors.As(err, &unsup):
		return "unsupported_by_provider"
	case errors.Is(err, ErrNeedsReauth):
		return CodeAuthExpired
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.As(err, &prov):
		return "provider_error"
	default:
		return "error"
	}
}

// Retryable reports whether retrying the same call could plausibly succeed.
func Retryable(err error) bool {
	var prov *ProviderError
	if errors.As(err, &prov) {
		return prov.Retryable
	}
	return false
}
