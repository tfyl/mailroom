package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Sessions is an in-process session store. Deliberately not a signed stateless cookie: a
// server-side record means logging out actually ends the session everywhere, and revoking
// access does not have to wait for a token to expire.
//
// The cost is that sessions do not survive a restart and are not shared between replicas.
// Both are acceptable for the single-binary default; a Redis-backed implementation is the
// scale-out path.
type Sessions struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]sessionEntry
}

type sessionEntry struct {
	op      Operator
	expires time.Time
}

func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Sessions{ttl: ttl, data: map[string]sessionEntry{}}
}

func (s *Sessions) TTL() time.Duration { return s.ttl }

func (s *Sessions) Create(op Operator) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.data[token] = sessionEntry{op: op, expires: time.Now().Add(s.ttl)}
	return token, nil
}

func (s *Sessions) Get(token string) (Operator, bool) {
	if token == "" {
		return Operator{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[token]
	if !ok {
		return Operator{}, false
	}
	if time.Now().After(e.expires) {
		delete(s.data, token)
		return Operator{}, false
	}
	return e.op, true
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, token)
}

func (s *Sessions) sweepLocked() {
	now := time.Now()
	for k, v := range s.data {
		if now.After(v.expires) {
			delete(s.data, k)
		}
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
