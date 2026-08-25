package aggregate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

type item struct {
	acct mail.AccountID
	date time.Time
	id   string
}

func dateOf(i item) time.Time         { return i.date }
func accountOf(i item) mail.AccountID { return i.acct }

func acct(id, alias string) mail.Account {
	return mail.Account{ID: mail.AccountID(id), Alias: alias, Status: mail.StatusLinked}
}

// pages builds a fetcher serving fixed pages per account, keyed by cursor.
func pages(data map[mail.AccountID][][]item) FetchFunc[item] {
	return func(_ context.Context, a mail.Account, cursor string) (mail.Page[item], error) {
		pp := data[a.ID]
		idx := 0
		if cursor != "" {
			fmt.Sscanf(cursor, "p%d", &idx)
		}
		if idx >= len(pp) {
			return mail.Page[item]{}, nil
		}
		next := ""
		if idx+1 < len(pp) {
			next = fmt.Sprintf("p%d", idx+1)
		}
		return mail.Page[item]{Items: pp[idx], Cursor: next}, nil
	}
}

func at(mins int) time.Time {
	return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(mins) * time.Minute)
}

func TestFanMergesNewestFirst(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	data := map[mail.AccountID][][]item{
		"a": {{{acct: "a", date: at(30), id: "a1"}, {acct: "a", date: at(10), id: "a2"}}},
		"b": {{{acct: "b", date: at(20), id: "b1"}}},
	}

	res, err := Fan(context.Background(), accounts, "", Options{Limit: 10}, dateOf, accountOf, pages(data))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, i := range res.Items {
		got = append(got, i.id)
	}
	want := []string{"a1", "b1", "a2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("want %v newest first, got %v", want, got)
	}
	if !res.Complete {
		t.Fatal("both accounts exhausted; result should be complete")
	}
}

// The limit is a total, not per account. A caller asking for 2 gets 2 however many mailboxes
// are linked.
func TestFanLimitIsTotalNotPerAccount(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	data := map[mail.AccountID][][]item{
		"a": {{{acct: "a", date: at(40), id: "a1"}, {acct: "a", date: at(30), id: "a2"}}},
		"b": {{{acct: "b", date: at(20), id: "b1"}, {acct: "b", date: at(10), id: "b2"}}},
	}

	res, err := Fan(context.Background(), accounts, "", Options{Limit: 2}, dateOf, accountOf, pages(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want exactly 2 items, got %d", len(res.Items))
	}
	if res.Complete {
		t.Fatal("items remain; result must not claim completeness")
	}
	if res.Cursor == "" {
		t.Fatal("an incomplete result must carry a cursor")
	}
}

// Items that lost the truncation must come back on the next page rather than being skipped.
func TestFanPaginationLosesNothing(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	data := map[mail.AccountID][][]item{
		"a": {{{acct: "a", date: at(40), id: "a1"}, {acct: "a", date: at(30), id: "a2"}}},
		"b": {{{acct: "b", date: at(20), id: "b1"}, {acct: "b", date: at(10), id: "b2"}}},
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 5; page++ {
		res, err := Fan(context.Background(), accounts, cursor, Options{Limit: 1}, dateOf, accountOf, pages(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range res.Items {
			seen[i.id]++
		}
		if res.Complete {
			break
		}
		cursor = res.Cursor
	}

	for _, id := range []string{"a1", "a2", "b1", "b2"} {
		if seen[id] != 1 {
			t.Fatalf("item %s seen %d times; every item must appear exactly once across pages", id, seen[id])
		}
	}
}

// One unhealthy mailbox must not lose the results from the healthy ones, but the failure has
// to be visible in the status block.
func TestFanPartialFailureKeepsGoodResults(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	fetch := func(_ context.Context, a mail.Account, _ string) (mail.Page[item], error) {
		if a.ID == "b" {
			return mail.Page[item]{}, &mail.ProviderError{
				Account: "home", Op: "search", Retryable: true, RetryIn: 30,
				Err: errors.New("429 too many requests"),
			}
		}
		return mail.Page[item]{Items: []item{{acct: "a", date: at(10), id: "a1"}}}, nil
	}

	res, err := Fan(context.Background(), accounts, "", Options{Limit: 10}, dateOf, accountOf, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("healthy account's results must survive, got %d", len(res.Items))
	}
	if res.Accounts["home"].Status != StatusRateLimited {
		t.Fatalf("failure must be visible, got %q", res.Accounts["home"].Status)
	}
	if res.Accounts["home"].RetryIn != 30 {
		t.Fatalf("retry hint should be reported, got %d", res.Accounts["home"].RetryIn)
	}
	if res.Accounts["work"].Status != StatusOK {
		t.Fatalf("healthy account should report ok, got %q", res.Accounts["work"].Status)
	}
	if res.Complete {
		t.Fatal("a failed account means the result is not complete")
	}
}

func TestFanAllFailedIsDetectable(t *testing.T) {
	accounts := []mail.Account{acct("a", "work")}
	fetch := func(_ context.Context, _ mail.Account, _ string) (mail.Page[item], error) {
		return mail.Page[item]{}, errors.New("boom")
	}

	res, err := Fan(context.Background(), accounts, "", Options{}, dateOf, accountOf, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() {
		t.Fatal("a result where every account failed must be distinguishable from an empty inbox")
	}
}

func TestFanExpiredCredentialsReportedDistinctly(t *testing.T) {
	accounts := []mail.Account{acct("a", "work")}
	fetch := func(_ context.Context, _ mail.Account, _ string) (mail.Page[item], error) {
		return mail.Page[item]{}, mail.ErrNeedsReauth
	}

	res, _ := Fan(context.Background(), accounts, "", Options{}, dateOf, accountOf, fetch)
	if res.Accounts["work"].Status != StatusAuthExpired {
		t.Fatalf("want auth_expired so the operator knows to re-link, got %q", res.Accounts["work"].Status)
	}
}

// Continuing a cursor against a different set of accounts would silently page through a
// different scope than the caller started with.
func TestCursorRejectedWhenScopeChanges(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	data := map[mail.AccountID][][]item{
		"a": {{{acct: "a", date: at(40), id: "a1"}, {acct: "a", date: at(30), id: "a2"}}},
		"b": {{{acct: "b", date: at(20), id: "b1"}}},
	}

	res, err := Fan(context.Background(), accounts, "", Options{Limit: 1}, dateOf, accountOf, pages(data))
	if err != nil {
		t.Fatal(err)
	}

	narrowed := []mail.Account{acct("a", "work")}
	if _, err := Fan(context.Background(), narrowed, res.Cursor, Options{Limit: 1}, dateOf, accountOf, pages(data)); !errors.Is(err, ErrCursorScope) {
		t.Fatalf("want ErrCursorScope, got %v", err)
	}
}

func TestMalformedCursorRejected(t *testing.T) {
	accounts := []mail.Account{acct("a", "work")}
	_, err := Fan(context.Background(), accounts, "!!!not base64!!!", Options{}, dateOf, accountOf, pages(nil))
	if !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("want ErrCursorMalformed, got %v", err)
	}
}

func TestFanTimeoutReportedPerAccount(t *testing.T) {
	accounts := []mail.Account{acct("a", "work"), acct("b", "home")}
	fetch := func(ctx context.Context, a mail.Account, _ string) (mail.Page[item], error) {
		if a.ID == "b" {
			<-ctx.Done()
			return mail.Page[item]{}, ctx.Err()
		}
		return mail.Page[item]{Items: []item{{acct: "a", date: at(5), id: "a1"}}}, nil
	}

	res, err := Fan(context.Background(), accounts, "",
		Options{Limit: 10, PerAccountTimeout: 30 * time.Millisecond}, dateOf, accountOf, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts["home"].Status != StatusTimeout {
		t.Fatalf("want timeout for the slow account, got %q", res.Accounts["home"].Status)
	}
	if len(res.Items) != 1 {
		t.Fatal("a slow account must not hold up the fast one's results")
	}
}
