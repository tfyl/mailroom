package oauthsrv

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tfyl/mailroom/internal/auth"
)

// registrationLimit bounds how much unauthenticated client registration an instance absorbs.
//
// Two counters, because one of them alone is the wrong control. A per-address bound is the
// one that keeps an ordinary instance usable while a single noisy client is refused, and it
// is also the one anybody with a botnet walks straight past. An instance-wide bound is the
// one that actually caps how many rows a stranger can put in the clients table in an hour,
// and it is also the one a flood can spend on everybody else's behalf. Together the cheap
// attack is refused per address and the expensive one still hits a ceiling.
//
// Order matters and is not an implementation detail. The instance counter is consulted first
// and the per-address map is only touched once it has room, which is what bounds this
// structure's memory: a caller cannot make the map grow past what the instance bound admits
// in one window, so a distributed flood cannot turn a rate limit into a way to exhaust the
// heap. Budget is spent only by a registration that is actually admitted, so refusals do not
// deepen a hole the honest caller then falls into.
type registrationLimit struct {
	perAddress rate
	instance   rate
	proxies    auth.TrustedProxies

	mu        sync.Mutex
	total     bucket
	addresses map[string]*bucket
}

// rate is a count within a window. Zero count means this half is not enforced.
type rate struct {
	count  int
	window time.Duration
}

func (r rate) on() bool { return r.count > 0 && r.window > 0 }

// newRegistrationLimit returns nil when neither half is configured, so an unbounded
// endpoint is a nil check rather than a limiter that always says yes.
func newRegistrationLimit(perAddress, instance rate, proxies auth.TrustedProxies) *registrationLimit {
	if !perAddress.on() && !instance.on() {
		return nil
	}
	return &registrationLimit{
		perAddress: perAddress,
		instance:   instance,
		proxies:    proxies,
		addresses:  map[string]*bucket{},
	}
}

// allow reports whether one more registration may be recorded for this request.
func (l *registrationLimit) allow(r *http.Request) bool {
	if l == nil {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.instance.on() && !l.total.room(now, l.instance) {
		return false
	}
	if l.perAddress.on() {
		key := addressKey(l.proxies.ClientIP(r))
		l.sweepLocked(now)
		b := l.addresses[key]
		if b == nil {
			b = &bucket{}
			l.addresses[key] = b
		}
		if !b.room(now, l.perAddress) {
			return false
		}
		b.record(now)
	}
	if l.instance.on() {
		l.total.record(now)
	}
	return true
}

// sweepLocked drops buckets that have gone quiet. The map is small by construction — the
// instance bound above caps how many distinct addresses can put an entry in it during one
// window — so a full pass costs less than deciding when to do a partial one.
func (l *registrationLimit) sweepLocked(now time.Time) {
	for key, b := range l.addresses {
		b.prune(now, l.perAddress.window)
		if len(b.at) == 0 {
			delete(l.addresses, key)
		}
	}
}

// bucket is the times of the admissions still inside a window. It holds at most the rate's
// count, because a refusal is never recorded: an over-budget caller hammering the endpoint
// must not push its own window forward and lock itself out for longer than the rate says.
type bucket struct {
	at []time.Time
}

func (b *bucket) room(now time.Time, r rate) bool {
	b.prune(now, r.window)
	return len(b.at) < r.count
}

func (b *bucket) record(now time.Time) { b.at = append(b.at, now) }

func (b *bucket) prune(now time.Time, window time.Duration) {
	cut := now.Add(-window)
	keep := b.at[:0]
	for _, t := range b.at {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	b.at = keep
}

// addressKey is the bucket an address falls in.
//
// An IPv6 address is masked to its /64 first. The smallest block anybody is assigned is a
// /64 and most are far larger, so counting single IPv6 addresses would be counting something
// the caller mints for free — a per-address bound that a caller can step around by picking a
// new address is not a bound. IPv4 is counted whole, because there the address is the scarce
// thing.
//
// An address that could not be established at all shares one bucket. That is a bucket
// everybody in it competes for, which is the restrictive direction, and it is not one a
// caller can choose to be in: the address comes from the connection, and nothing a request
// carries can make its own source unparseable.
func addressKey(addr string) string {
	if addr == "" {
		return "unattributable"
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if ip.To4() != nil {
		return ip.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
