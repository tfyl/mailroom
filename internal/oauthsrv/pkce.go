package oauthsrv

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/tfyl/mailroom/internal/user"
)

// verifyPKCE checks a code verifier against the challenge recorded at authorization time.
//
// Only S256 is accepted. The "plain" method offers no protection against an intercepted
// authorization code, and OAuth 2.1 drops it; accepting it here would let a client opt out
// of the protection silently.
func verifyPKCE(challenge, method, verifier string) error {
	if method != "S256" {
		return fmt.Errorf("code_challenge_method must be S256")
	}
	if verifier == "" {
		return fmt.Errorf("code_verifier is required")
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) != 1 {
		return fmt.Errorf("code_verifier does not match the challenge")
	}
	return nil
}

// pendingAuth is an authorization request awaiting the operator's decision, and then the
// short-lived code the client exchanges.
type pendingAuth struct {
	// Owner is the user who was shown the consent screen. Approval checks that the session
	// approving is still the same one, so a leaked request id cannot be redeemed elsewhere.
	Owner         user.ID
	ClientID      string
	RedirectURI   string
	State         string
	Challenge     string
	ChallengeAlgo string
	RequestedCaps []string // what the client asked for; displayed, never preselected
	GrantID       string   // set once the operator approves
	expires       time.Time
}

// codeStore holds authorization requests and issued codes. In-process by design: these live
// for seconds, and persisting them would mean writing a credential to disk to save a state
// that a client retry recreates anyway.
type codeStore struct {
	mu   sync.Mutex
	data map[string]*pendingAuth
	ttl  time.Duration
}

func newCodeStore(ttl time.Duration) *codeStore {
	return &codeStore{data: map[string]*pendingAuth{}, ttl: ttl}
}

func (c *codeStore) put(key string, p *pendingAuth) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	p.expires = time.Now().Add(c.ttl)
	c.data[key] = p
}

func (c *codeStore) get(key string) (*pendingAuth, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.data[key]
	if !ok || time.Now().After(p.expires) {
		delete(c.data, key)
		return nil, false
	}
	return p, true
}

// take fetches and removes in one step. Authorization codes are single-use: replaying one
// must not yield a second token.
func (c *codeStore) take(key string) (*pendingAuth, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.data[key]
	delete(c.data, key)
	if !ok || time.Now().After(p.expires) {
		return nil, false
	}
	return p, true
}

func (c *codeStore) sweepLocked() {
	now := time.Now()
	for k, v := range c.data {
		if now.After(v.expires) {
			delete(c.data, k)
		}
	}
}
