package web

import (
	"sync"
	"time"

	"github.com/tfyl/mailroom/internal/user"
)

// linkStore holds in-flight mailbox linking attempts: the OAuth state parameter mapped to
// the alias the operator typed and the user who typed it. In-process and short-lived,
// because a linking attempt that does not complete within minutes should be restarted
// rather than resumed.
type linkStore struct {
	mu   sync.Mutex
	data map[string]linkAttempt
	ttl  time.Duration
}

// linkAttempt records who started the attempt, because the callback is a top-level GET and
// arrives with whatever session the browser happens to hold. Without the owner here, an
// attacker can complete Google's consent for a mailbox they control, hand the callback URL
// to a signed-in victim, and have their own mailbox linked into the victim's account — a
// mailbox they can write into and read the agent's replies out of.
type linkAttempt struct {
	Owner   user.ID
	Alias   string
	expires time.Time
}

func newLinkStore(ttl time.Duration) *linkStore {
	return &linkStore{data: map[string]linkAttempt{}, ttl: ttl}
}

func (l *linkStore) put(state string, a linkAttempt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, v := range l.data {
		if now.After(v.expires) {
			delete(l.data, k)
		}
	}
	a.expires = now.Add(l.ttl)
	l.data[state] = a
}

// take consumes the state, so a replayed callback cannot link a second mailbox, and hands
// the alias back only to the user who started the attempt. An unknown state, an expired one
// and one belonging to somebody else are all the same answer: which of the three it was is
// not the caller's business.
func (l *linkStore) take(state string, owner user.ID) (string, bool) {
	if state == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.data[state]
	delete(l.data, state)
	if !ok || time.Now().After(a.expires) || a.Owner != owner {
		return "", false
	}
	return a.Alias, true
}
