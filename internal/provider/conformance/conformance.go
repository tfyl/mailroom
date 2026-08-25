// Package conformance is the contract every mail provider must satisfy.
//
// It exists because a provider that compiles is not a provider that works, and because an
// abstraction validated against a single implementation quietly encodes that
// implementation's assumptions — which is the exact failure it is supposed to prevent.
//
// The suite comes in two halves. Static needs no credentials and checks the structural
// claims a provider makes about itself; a new provider can pass it on the day it is written.
// Live needs a real mailbox and checks behaviour. Both are the contract; Static is simply
// the part that can run in CI without secrets.
package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tfyl/mailroom/internal/mail"
)

// Static checks the claims a provider makes about itself, without touching the network.
//
// The central one: Capabilities must agree with the interfaces actually implemented. A
// capability set that overstates is worse than one that is merely narrow — a caller trusts
// it to decide what to attempt, and an overstated set turns a clear "unsupported" into a
// confusing runtime failure.
func Static(t *testing.T, p mail.Provider) {
	t.Helper()

	t.Run("identifies itself", func(t *testing.T) {
		if p.ID() == "" {
			t.Error("ID() must return a non-empty provider id")
		}
	})

	// The claimed set must be a subset of what is implemented, not equal to it.
	//
	// Overstating is the failure that matters: a caller trusts this set to decide what to
	// attempt, and a claim with nothing behind it turns a clear refusal into a runtime
	// surprise. Understating is legitimate and sometimes required — a provider whose sending
	// depends on configuration must implement MessageWriter to satisfy the interface while
	// withholding the capability on an account that has no SMTP host.
	t.Run("capabilities do not overstate what is implemented", func(t *testing.T) {
		claimed := p.Capabilities()

		for _, c := range mail.AllCapabilities {
			if claimed.Has(c) && !mail.Supports(p, c) {
				t.Errorf("Capabilities() claims %q but the interface backing it is not implemented", c)
			}
		}
	})

	// The optional settings interfaces are reachable only through a provider that also
	// implements SettingsManager, because CapSettings is what gates the tool. One implemented
	// without it would be dead code the caller can never reach — which looks like support and
	// behaves like absence.
	t.Run("optional settings interfaces are reachable", func(t *testing.T) {
		_, core := p.(mail.SettingsManager)
		if core {
			return
		}
		for name, implemented := range map[string]bool{
			"DelegateManager":    implementsDelegates(p),
			"ForwardingReader":   implementsForwarding(p),
			"IMAPSettingsReader": implementsIMAPSettings(p),
		} {
			if implemented {
				t.Errorf("%s is implemented but SettingsManager is not, so nothing can reach it", name)
			}
		}
	})

	t.Run("claims at least one capability", func(t *testing.T) {
		if p.Capabilities().Len() == 0 {
			t.Error("a provider that can do nothing is not usable; implement at least MessageReader")
		}
	})

	t.Run("quirks are recognised values", func(t *testing.T) {
		known := map[mail.Quirk]bool{
			mail.QuirkDerivedThreads: true,
			mail.QuirkExclusiveLabel: true,
			mail.QuirkNoBatch:        true,
			mail.QuirkPartialSearch:  true,
			mail.QuirkUnstablePaging: true,
		}
		for _, q := range p.Quirks() {
			if !known[q] {
				t.Errorf("unknown quirk %q: callers cannot interpret it, so add it to the model first", q)
			}
		}
	})

	// A provider deriving threads from headers must say so. An agent told to "reply to the
	// last message in this thread" needs to know whether the grouping was authoritative or
	// inferred, and it can only learn that from the quirk.
	t.Run("derived threading is declared", func(t *testing.T) {
		if _, ok := p.(mail.ThreadReader); !ok {
			t.Skip("provider does not read threads")
		}
		// Nothing to assert without a live call; Live checks the flag on real results. This
		// case exists so the requirement is visible in the suite itself.
	})
}

// Harness supplies a live mailbox for the behavioural half of the suite.
type Harness struct {
	Provider mail.Provider

	// Account is the mailbox under test. Its id must match the one the provider stamps into
	// the ids it returns.
	Account mail.Account

	// SearchAll is a query expected to match at least three messages, so pagination has
	// something to page through.
	SearchAll mail.Query

	// MissingID is a well-formed identifier for a message that does not exist.
	//
	// The suite cannot synthesize this. A bare string is a plausible missing id for Gmail and
	// a *malformed* one for IMAP and Zoho, whose ids carry a mailbox or folder as well — and
	// malformed and missing are different failures with different fixes. Asking the provider
	// for one is what keeps this check from quietly encoding one provider's id shape.
	MissingID mail.ScopedID

	// SkipDestructive leaves trash and delete untested. Set it when the suite is pointed at
	// a mailbox whose contents matter.
	SkipDestructive bool
}

// Live exercises behaviour against a real mailbox. It is slow, needs credentials, and is the
// only way to catch the failures that matter most.
func Live(t *testing.T, h Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reader, ok := h.Provider.(mail.MessageReader)
	if !ok {
		t.Fatal("a provider must implement MessageReader to be useful")
	}

	// An empty answer here is a failure, not a reason to stop.
	//
	// It used to skip, and that is how the IMAP provider passed this suite for months while
	// returning no results for every search anybody could make: it sent SEARCH and read the
	// answer as UIDs, which is empty, and the skip then took every behavioural check after
	// this one with it. A suite that stands down when the provider finds nothing cannot
	// distinguish an empty mailbox from a provider that finds nothing ever — which is the
	// same confusion, one layer up, that this whole contract exists to prevent.
	//
	// The harness promises SearchAll matches at least three messages. Holding it to that is
	// what makes the rest of the suite mean anything.
	t.Run("search returns results carrying this account", func(t *testing.T) {
		page, err := reader.Search(ctx, h.SearchAll, "")
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(page.Items) == 0 {
			t.Fatal("the search matched nothing. SearchAll is documented as a query matching " +
				"at least three messages, so either the harness is pointed at the wrong " +
				"mailbox or this provider cannot search — and the second is not something " +
				"the suite may skip past")
		}
		for _, m := range page.Items {
			if m.ID.Account != h.Account.ID {
				t.Errorf("message id is stamped with %q, want the account under test %q",
					m.ID.Account, h.Account.ID)
			}
			if m.ID.Native == "" {
				t.Error("message id has no provider-native part")
			}
			if m.Date.IsZero() {
				t.Error("message has no date; the aggregator sorts on it and would order this arbitrarily")
			}
		}
	})

	// Ids that cannot be resolved are the single most common way a provider looks fine in a
	// listing and fails the moment anything acts on a result.
	t.Run("search ids resolve through get", func(t *testing.T) {
		page, err := reader.Search(ctx, h.SearchAll, "")
		if err != nil || len(page.Items) == 0 {
			t.Skip("the search above already failed; fix that first")
		}
		first := page.Items[0]

		got, err := reader.Get(ctx, first.ID)
		if err != nil {
			t.Fatalf("an id returned by Search was not resolvable by Get: %v", err)
		}
		if got.ID.String() != first.ID.String() {
			t.Errorf("Get returned id %s for a request for %s", got.ID, first.ID)
		}
	})

	t.Run("pagination terminates and does not repeat", func(t *testing.T) {
		q := h.SearchAll
		q.Limit = 1

		seen := map[string]int{}
		cursor := ""
		for page := 0; page < 5; page++ {
			res, err := reader.Search(ctx, q, cursor)
			if err != nil {
				t.Fatalf("page %d failed: %v", page, err)
			}
			for _, m := range res.Items {
				seen[m.ID.String()]++
			}
			if res.Cursor == "" {
				break
			}
			if res.Cursor == cursor {
				t.Fatal("cursor did not advance; paging would loop forever")
			}
			cursor = res.Cursor
		}
		for id, n := range seen {
			if n > 1 {
				t.Errorf("message %s appeared on %d pages; pagination must not repeat", id, n)
			}
		}
	})

	// An empty result and a failure must be distinguishable. A provider that returns an empty
	// page on error makes an unreachable mailbox look like an empty one, and a model will
	// confidently tell its user there is no such mail.
	t.Run("no matches is not an error", func(t *testing.T) {
		res, err := reader.Search(ctx, mail.Query{
			Raw:   "subject:zzz-nothing-should-ever-match-this-zzz",
			Limit: 5,
		}, "")
		if err != nil {
			t.Fatalf("a query matching nothing must return an empty page, not an error: %v", err)
		}
		if len(res.Items) != 0 {
			t.Errorf("expected no matches, got %d", len(res.Items))
		}
	})

	t.Run("missing message reports not found", func(t *testing.T) {
		if h.MissingID.Zero() {
			t.Skip("harness supplied no MissingID; see the field comment")
		}
		_, err := reader.Get(ctx, h.MissingID)
		if err == nil {
			t.Fatal("fetching a nonexistent message must fail")
		}
		if !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("want ErrNotFound so callers can distinguish it from a transport failure, got %T: %v", err, err)
		}
	})

	liveThreads(ctx, t, h)
	liveLabels(ctx, t, h)
	liveFilters(ctx, t, h)
	liveSettings(ctx, t, h)
	liveUnsupported(ctx, t, h)
}

func liveThreads(ctx context.Context, t *testing.T, h Harness) {
	reader, ok := h.Provider.(mail.ThreadReader)
	if !ok {
		return
	}
	messages, _ := h.Provider.(mail.MessageReader)

	t.Run("thread declares whether it was derived", func(t *testing.T) {
		page, err := messages.Search(ctx, h.SearchAll, "")
		if err != nil || len(page.Items) == 0 {
			t.Skip("the search above already failed; fix that first")
		}

		thread, err := reader.GetThread(ctx, page.Items[0].ThreadID)
		if err != nil {
			t.Fatalf("get thread failed: %v", err)
		}
		if len(thread.Messages) == 0 {
			t.Error("a thread must contain at least the message it was reached from")
		}

		// Derived must be true exactly when the provider declared the quirk. Getting this
		// backwards is silent and misleads every agent that reasons about conversations.
		declared := false
		for _, q := range h.Provider.Quirks() {
			if q == mail.QuirkDerivedThreads {
				declared = true
			}
		}
		if thread.Derived != declared {
			t.Errorf("Thread.Derived is %v but the provider %s declare the derived_threads quirk",
				thread.Derived, map[bool]string{true: "does", false: "does not"}[declared])
		}
	})
}

// liveFilters reads the existing filters. Read-only: creating one changes how the mailbox
// treats mail arriving later, which is not a side effect a contract test should leave behind.
func liveFilters(ctx context.Context, t *testing.T, h Harness) {
	manager, ok := h.Provider.(mail.FilterManager)
	if !ok {
		return
	}

	t.Run("filters are listable and well formed", func(t *testing.T) {
		filters, err := manager.ListFilters(ctx)
		if err != nil {
			t.Fatalf("listing filters failed: %v", err)
		}
		for _, f := range filters {
			if f.ID == "" {
				t.Errorf("filter %+v has no id, so it cannot be deleted", f)
			}
		}
	})
}

// liveSettings reads each settings section the provider claims. Nothing is written: an
// auto-reply or a delegation left behind by a test is somebody's real mailbox misbehaving.
func liveSettings(ctx context.Context, t *testing.T, h Harness) {
	manager, ok := h.Provider.(mail.SettingsManager)
	if !ok {
		return
	}

	t.Run("send-as aliases are listable", func(t *testing.T) {
		aliases, err := manager.ListSendAs(ctx)
		if err != nil {
			t.Fatalf("listing aliases failed: %v", err)
		}
		// Every mailbox can send as itself, so an empty list means the call did not work
		// rather than that there is nothing to report.
		if len(aliases) == 0 {
			t.Error("expected at least the primary address")
		}
		var primary int
		for _, a := range aliases {
			if a.Address == "" {
				t.Errorf("alias %+v has no address", a)
			}
			if a.Primary {
				primary++
			}
		}
		if primary > 1 {
			t.Errorf("a mailbox has one primary address, got %d", primary)
		}
	})

	t.Run("vacation responder is readable", func(t *testing.T) {
		if _, err := manager.GetVacation(ctx); err != nil {
			t.Fatalf("reading the vacation responder failed: %v", err)
		}
	})
}

func liveLabels(ctx context.Context, t *testing.T, h Harness) {
	manager, ok := h.Provider.(mail.LabelManager)
	if !ok {
		return
	}

	t.Run("labels declare their exclusivity honestly", func(t *testing.T) {
		labels, err := manager.ListLabels(ctx)
		if err != nil {
			t.Fatalf("list labels failed: %v", err)
		}

		declared := false
		for _, q := range h.Provider.Quirks() {
			if q == mail.QuirkExclusiveLabel {
				declared = true
			}
		}

		var anyExclusive bool
		for _, l := range labels {
			if l.Exclusive {
				anyExclusive = true
			}
			if l.ID == "" || l.Name == "" {
				t.Errorf("label %+v is missing an id or a name", l)
			}
		}

		// A provider whose labels move messages must warn callers, because applying one is
		// then destructive of the previous placement rather than additive.
		if anyExclusive && !declared {
			t.Error("provider returns exclusive labels but does not declare the exclusive_labels quirk")
		}
	})

	// A provider that can bin mail has to be able to say which of its labels does it.
	//
	// This is the contract behind the destructive-label check in internal/mcp: applying a
	// label is trashing on every provider here, and the permission model can only tell the two
	// apart because the provider classifies its own ids. A provider that implements Destroyer —
	// that has a bin at all — and classifies every label it lists as ordinary has either
	// hidden its bin from ListLabels or answered EffectOfApplying without meaning it, and
	// either way `destructive` no longer gates trashing on that mailbox.
	t.Run("the bin is classified as destructive", func(t *testing.T) {
		if _, ok := h.Provider.(mail.Destroyer); !ok {
			t.Skip("provider has no bin of its own")
		}
		labels, err := manager.ListLabels(ctx)
		if err != nil {
			t.Fatalf("list labels failed: %v", err)
		}

		var named []string
		for _, l := range labels {
			effect, err := manager.EffectOfApplying(ctx, l.ID)
			if err != nil {
				t.Fatalf("classifying %q: %v", l.ID, err)
			}
			if effect.Destructive() {
				named = append(named, string(l.ID)+"="+string(effect))
			}
		}
		if len(named) == 0 {
			t.Errorf("this provider can trash mail and classifies none of its %d labels as "+
				"destructive, so applying one is a way past the destructive capability",
				len(labels))
		}
		t.Logf("labels that destroy mail when applied: %v", named)
	})

	// Refusing an unsupported request is correct. Silently creating something that behaves
	// differently from what was asked for is not: the caller only finds out much later.
	t.Run("exclusive label creation is refused when unsupported", func(t *testing.T) {
		supportsExclusive := false
		for _, q := range h.Provider.Quirks() {
			if q == mail.QuirkExclusiveLabel {
				supportsExclusive = true
			}
		}
		if supportsExclusive {
			t.Skip("provider supports exclusive labels")
		}

		_, err := manager.CreateLabel(ctx, "mailroom-conformance-should-not-exist", true)
		if err == nil {
			t.Fatal("a provider without exclusive labels must refuse to create one, not create a normal label")
		}
		var unsupported *mail.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Errorf("want UnsupportedError so the caller can tell this apart from a failure, got %T", err)
		}
	})
}

// liveUnsupported checks that absent capabilities fail in the one way callers can act on.
func liveUnsupported(_ context.Context, t *testing.T, h Harness) {
	t.Run("unimplemented capabilities are absent, not stubbed", func(t *testing.T) {
		caps := h.Provider.Capabilities()
		for _, c := range mail.AllCapabilities {
			if caps.Has(c) && !mail.Supports(h.Provider, c) {
				t.Errorf("capability %q is claimed but not implemented", c)
			}
		}
	})
}

func implementsDelegates(p mail.Provider) bool {
	_, ok := p.(mail.DelegateManager)
	return ok
}

func implementsForwarding(p mail.Provider) bool {
	_, ok := p.(mail.ForwardingReader)
	return ok
}

func implementsIMAPSettings(p mail.Provider) bool {
	_, ok := p.(mail.IMAPSettingsReader)
	return ok
}
