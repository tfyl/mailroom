// Package aggregate fans a single request out across several mailboxes and merges the
// answers into one result.
//
// Two properties matter more than throughput. First, one unhealthy mailbox must never lose
// the results from the healthy ones. Second, a degraded answer must be visibly degraded: a
// caller that cannot distinguish "no matching mail" from "that mailbox was unreachable" will
// confidently tell its user the wrong thing.
package aggregate

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

type Status string

const (
	StatusOK          Status = "ok"
	StatusRateLimited Status = "rate_limited"
	StatusAuthExpired Status = "auth_expired"
	StatusUnsupported Status = "unsupported"
	StatusTimeout     Status = "timeout"
	StatusError       Status = "error"
)

// AccountStatus is the per-account outcome reported alongside every aggregated result.
//
// The block stays keyed by alias, because that is what a caller selects by and what each
// merged row names its mailbox with. Address says which mailbox that alias currently is, so
// a caller reading "archive returned nothing" can tell whether archive is the one it meant.
type AccountStatus struct {
	Status   Status `json:"status"`
	Address  string `json:"address,omitempty"`
	Returned int    `json:"returned"`
	Message  string `json:"message,omitempty"`
	RetryIn  int    `json:"retry_in,omitempty"`
}

// Result is the envelope every fan-out capable tool returns, including when only one account
// is in scope — a client should never have to handle two response shapes for one tool.
type Result[T any] struct {
	Items    []T                      `json:"results"`
	Accounts map[string]AccountStatus `json:"accounts"`
	Cursor   string                   `json:"cursor,omitempty"`
	Complete bool                     `json:"complete"`
}

// Failed reports whether every account touched failed. Callers turn this into an error
// rather than returning an empty success, which would read as "no mail found".
func (r Result[T]) Failed() bool {
	if len(r.Accounts) == 0 {
		return false
	}
	for _, s := range r.Accounts {
		if s.Status == StatusOK {
			return false
		}
	}
	return true
}

// FetchFunc retrieves one page from one account. The cursor is that account's own, opaque to
// everything above the provider.
type FetchFunc[T any] func(ctx context.Context, acct mail.Account, cursor string) (mail.Page[T], error)

// Options tune a fan-out. The zero value is usable.
type Options struct {
	// Limit is the total across all accounts, not per account. A caller asking for 20
	// results gets 20, however many mailboxes happen to be linked.
	Limit int
	// PerAccountTimeout bounds each account independently, so one slow provider degrades to
	// a timeout in the status block instead of holding the whole response.
	PerAccountTimeout time.Duration
}

const (
	defaultLimit   = 25
	defaultTimeout = 20 * time.Second
)

func (o Options) limit() int {
	if o.Limit <= 0 {
		return defaultLimit
	}
	return o.Limit
}

func (o Options) timeout() time.Duration {
	if o.PerAccountTimeout <= 0 {
		return defaultTimeout
	}
	return o.PerAccountTimeout
}

// Fan queries every account concurrently, merges the results newest first, and truncates to
// the requested limit.
//
// dateOf extracts the sort key. Ties break on the item's account so that equal timestamps do
// not reorder between calls — an unstable sort here would make pagination skip or repeat.
func Fan[T any](
	ctx context.Context,
	accounts []mail.Account,
	cursor string,
	opts Options,
	dateOf func(T) time.Time,
	accountOf func(T) mail.AccountID,
	fetch FetchFunc[T],
) (Result[T], error) {
	state, err := decodeCursor(cursor, accounts)
	if err != nil {
		return Result[T]{}, err
	}

	limit := opts.limit()

	type fetched struct {
		acct   mail.Account
		items  []T
		next   string
		status AccountStatus
	}

	results := make([]fetched, len(accounts))
	var wg sync.WaitGroup

	for i, acct := range accounts {
		pos := state.Accounts[string(acct.ID)]
		if pos.Done {
			results[i] = fetched{acct: acct, status: AccountStatus{Status: StatusOK}}
			continue
		}

		wg.Add(1)
		go func(i int, acct mail.Account, pos position) {
			defer wg.Done()

			actx, cancel := context.WithTimeout(ctx, opts.timeout())
			defer cancel()

			page, err := fetch(actx, acct, pos.Cursor)
			if err != nil {
				results[i] = fetched{acct: acct, status: classify(actx, err)}
				return
			}

			// Skip the items already consumed from this page on a previous call. Re-fetching
			// a page to skip into it costs a request, and buys a stateless cursor that
			// carries no message data — worth it, since the alternative is either
			// server-side paging state or leftover items embedded in the cursor.
			items := page.Items
			if pos.Skip > 0 && pos.Skip < len(items) {
				items = items[pos.Skip:]
			} else if pos.Skip >= len(items) {
				items = nil
			}

			results[i] = fetched{
				acct:   acct,
				items:  items,
				next:   page.Cursor,
				status: AccountStatus{Status: StatusOK},
			}
		}(i, acct, pos)
	}
	wg.Wait()

	// Merge. Every fetched item is a candidate; the sort is stable on the account ID so
	// identical timestamps keep a deterministic order across pages.
	type candidate struct {
		item T
		from int
	}
	var all []candidate
	for i, r := range results {
		for _, it := range r.items {
			all = append(all, candidate{item: it, from: i})
		}
	}
	sort.SliceStable(all, func(x, y int) bool {
		dx, dy := dateOf(all[x].item), dateOf(all[y].item)
		if dx.Equal(dy) {
			return accountOf(all[x].item) < accountOf(all[y].item)
		}
		return dx.After(dy)
	})

	if len(all) > limit {
		all = all[:limit]
	}

	// Record how much of each account's page was consumed, so the next call resumes exactly
	// where this one stopped.
	consumed := make([]int, len(results))
	out := make([]T, 0, len(all))
	for _, c := range all {
		out = append(out, c.item)
		consumed[c.from]++
	}

	next := cursorState{Accounts: map[string]position{}, Scope: state.Scope}
	complete := true
	statuses := make(map[string]AccountStatus, len(results))

	for i, r := range results {
		st := r.status
		st.Returned = consumed[i]
		st.Address = r.acct.Address
		statuses[r.acct.Alias] = st

		pos := state.Accounts[string(r.acct.ID)]
		switch {
		case st.Status != StatusOK:
			// A failed account is not finished — it failed. Preserve its position so a retry
			// resumes rather than restarting, and keep the overall result incomplete.
			next.Accounts[string(r.acct.ID)] = pos
			complete = false
		case consumed[i] < len(r.items):
			// Part of this page survived the truncation; resume inside it.
			next.Accounts[string(r.acct.ID)] = position{
				Cursor: pos.Cursor,
				Skip:   pos.Skip + consumed[i],
			}
			complete = false
		case r.next != "":
			next.Accounts[string(r.acct.ID)] = position{Cursor: r.next}
			complete = false
		default:
			next.Accounts[string(r.acct.ID)] = position{Done: true}
		}
	}

	res := Result[T]{Items: out, Accounts: statuses, Complete: complete}
	if !complete {
		encoded, err := encodeCursor(next)
		if err != nil {
			return Result[T]{}, err
		}
		res.Cursor = encoded
	}
	return res, nil
}

// classify turns a provider failure into a status a caller can act on. A timeout is reported
// as a timeout rather than a generic error because retrying is reasonable; an expired
// credential is reported distinctly because retrying is not — an operator has to re-link.
func classify(ctx context.Context, err error) AccountStatus {
	if ctx.Err() == context.DeadlineExceeded {
		return AccountStatus{Status: StatusTimeout, Message: "account did not respond in time"}
	}
	switch mail.Code(err) {
	case "auth_expired":
		return AccountStatus{Status: StatusAuthExpired, Message: "credentials expired; re-link this mailbox"}
	case "unsupported_by_provider":
		return AccountStatus{Status: StatusUnsupported, Message: err.Error()}
	}
	st := AccountStatus{Status: StatusError, Message: err.Error()}
	if mail.Retryable(err) {
		st.Status = StatusRateLimited
		var pe *mail.ProviderError
		if asProviderError(err, &pe) {
			st.RetryIn = pe.RetryIn
		}
	}
	return st
}
