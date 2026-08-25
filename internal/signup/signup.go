// Package signup decides who is allowed to become a user of an instance.
//
// This is a separate question from authentication. An identity provider proves who somebody
// is; it does not necessarily decide whether they belong here. When the issuer is an
// Authentik or Keycloak the operator runs, it does decide, and the right policy is to
// inherit its answer. When the issuer is `accounts.google.com`, it decides nothing at all:
// every Google account in existence authenticates successfully, so an instance with no
// policy of its own accepts all of them.
//
// Hence a policy that defaults to refusing everyone except the first person to arrive.
package signup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
)

// Mode is the instance's answer to who may sign up.
type Mode string

const (
	// Closed admits nobody new. Identities that already have a user row sign in as usual,
	// and the very first sign-in still claims the instance — otherwise a fresh deployment
	// could never be used at all.
	Closed Mode = "closed"

	// Invite admits somebody holding an unredeemed invite code.
	Invite Mode = "invite"

	// Allowlist admits addresses or domains named in the configuration.
	Allowlist Mode = "allowlist"

	// Open admits anybody the issuer authenticates. Correct only where the issuer is
	// already the gate.
	Open Mode = "open"
)

// Policy is the loaded configuration. The zero value is Closed, which is the intended
// default: an instance that quietly accepts strangers is a mistake discovered late.
type Policy struct {
	Mode    Mode
	Emails  []string
	Domains []string
}

// ParseMode reads a configured mode, rejecting anything it does not recognise rather than
// falling back. A typo silently becoming `open` is the failure this is here to prevent.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return Closed, nil
	case Closed, Invite, Allowlist, Open:
		return m, nil
	default:
		return "", fmt.Errorf("must be one of closed, invite, allowlist, open; got %q", s)
	}
}

// NewPolicy normalises the allowlist so matching does not have to.
func NewPolicy(mode Mode, emails, domains []string) Policy {
	p := Policy{Mode: mode}
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			p.Emails = append(p.Emails, e)
		}
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "@")
		if d != "" {
			p.Domains = append(p.Domains, d)
		}
	}
	return p
}

// AllowsEmail reports whether an address is named by the allowlist.
//
// It trusts the address the issuer supplied. That trust is only sound when the issuer
// verifies addresses, so an allowlist against an issuer that does not is worth no more than
// the issuer's word — configure a required claim such as `email_verified=true` alongside it.
func (p Policy) AllowsEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	for _, e := range p.Emails {
		if e == email {
			return true
		}
	}
	_, domain, found := strings.Cut(email, "@")
	if !found {
		return false
	}
	for _, d := range p.Domains {
		if d == domain {
			return true
		}
	}
	return false
}

// Describe renders the policy for the operator interface.
func (p Policy) Describe() string {
	switch p.Mode {
	case Open:
		return "Anyone your identity provider authenticates can create an account here."
	case Allowlist:
		return fmt.Sprintf("New accounts are limited to %d address(es) and %d domain(s) named in the configuration.",
			len(p.Emails), len(p.Domains))
	case Invite:
		return "New accounts require an invite."
	default:
		return "This instance is not accepting new accounts."
	}
}

var codeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewCode returns an invite code: 160 bits, in an alphabet without the characters people
// mistake for one another, because these get read aloud and retyped.
func NewCode() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return codeEncoding.EncodeToString(b), nil
}

// HashCode is how an invite is stored. Only the hash is kept, so a copy of the database
// hands over no usable invites.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(NormalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// NormalizeCode absorbs the ways a code arrives after a round trip through a chat message:
// lowercased, spaced out, or wrapped in whitespace.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.Join(strings.Fields(code), ""))
}
