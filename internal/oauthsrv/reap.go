package oauthsrv

import (
	"context"
	"log/slog"
	"time"
)

// ClientSweepInterval is how often abandoned registrations are reclaimed. A constant rather
// than a setting, on the same reasoning as the held queue's: the interval decides only how
// far past its TTL a dead row lingers, and an operator with an opinion about how long a
// registration should survive wants the TTL.
const ClientSweepInterval = time.Hour

// SweepClients deletes registrations older than ttl that never became a grant.
//
// The rate limit on POST /register bounds the slope; this bounds the total. Without it an
// instance still accumulates every registration anybody ever made, only more slowly, and
// "more slowly" is not a bound. What it removes is provably garbage: a client row that no
// grant references was never approved by anybody, and registering confers nothing on its own.
//
// A TTL of zero disables it, and an instance that would rather keep every row it has ever
// seen can ask for that.
func (s *Server) SweepClients(ctx context.Context, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	return s.store.DeleteUnusedClients(ctx, s.now().Add(-ttl))
}

// SweepClientsEvery runs the reclaimer until the context ends, starting with one pass
// immediately. Same shape, and for the same reason, as the attachment and held-action
// sweepers: a restart after downtime is exactly when there is a backlog to clear.
func (s *Server) SweepClientsEvery(ctx context.Context, every, ttl time.Duration, log *slog.Logger) {
	if ttl <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := s.SweepClients(ctx, ttl); err != nil {
			log.Warn("could not reclaim unused client registrations", "err", err)
		} else if n > 0 {
			log.Info("reclaimed client registrations that never became a grant",
				"count", n, "ttl", ttl.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
