package conformance

import (
	"errors"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/mail"
)

// This half of the suite checks what a provider *sends*, not what it does with what comes
// back. The rest of the file exists because of one bug, and the shape of that bug is worth
// stating rather than paraphrasing.
//
// Zoho's search expression was built as `field:contains:value` joined by `&&`, with free
// text emitted as a bare word carrying no field at all. Zoho's syntax is `field:value`
// joined by `::`, with `entire:` for a whole-message search. Zoho parsed none of it and
// answered success with zero results, so every plain-language search against a Zoho mailbox
// silently found nothing — and the conformance suite passed throughout, because it tested
// mailroom's expectations against stubs that answered whatever they were told to. A stub
// cannot reject syntax the real service rejects. Nothing in a round trip through one is
// evidence about the service at all.
//
// So: a table of canonical queries, and, per provider, the exact thing that has to appear in
// the request each one produces — or a refusal, where the provider genuinely cannot express
// it. Every cell carries a citation, and a term with no cell at all is a failure rather than
// a gap, so adding a term to the table forces all four providers to answer for it.
//
// What this cannot do is confirm that the wire form is *right*. That still comes from the
// documentation, which is why Expectation refuses to be written without saying which piece
// of documentation it came from. What it does do is make a translation visible, name it
// beside its source, and stop a fifth provider being written with a silent hole in it.

// QueryTerm is one canonical search request. The name is what a provider's expectations are
// keyed by, so it is part of the contract rather than a label.
type QueryTerm struct {
	Name  string
	Query mail.Query
}

// The values are nonsense words on purpose: a marker a provider is asserted to have sent
// should not be a word that could turn up in the request for some other reason.
const (
	textTerm    = "sasquatch"
	senderTerm  = "hedgehog@example.com"
	toTerm      = "badger@example.com"
	subjectTerm = "aardvark"
)

// QueryTerms is every search mail_search can ask for, one term at a time and then in the
// combinations that providers disagree about.
//
// The combinations are here because that is where three of the four diverge. A filter that
// works alone can become unexpressible next to free text — Graph will not evaluate $filter
// beside $search, and Zoho's search endpoint reads none of the listing endpoint's filter
// parameters — and the failure mode is not an error, it is a full unfiltered page that looks
// exactly like an answer.
//
// Date bounds and label scoping are deliberately absent: mail_search exposes neither, so no
// caller can reach them. They are covered per provider instead, and the day they become tool
// arguments they belong here.
var QueryTerms = []QueryTerm{
	{"free text", mail.Query{Raw: textTerm}},
	{"sender", mail.Query{From: senderTerm}},
	{"recipient", mail.Query{To: toTerm}},
	{"subject", mail.Query{Subject: subjectTerm}},
	{"unread", mail.Query{Unread: true}},
	{"starred", mail.Query{Starred: true}},
	{"has attachment", mail.Query{HasAttach: true}},
	{"unread alongside free text", mail.Query{Raw: textTerm, Unread: true}},
	{"starred alongside free text", mail.Query{Raw: textTerm, Starred: true}},
	{"attachment alongside free text", mail.Query{Raw: textTerm, HasAttach: true}},
}

// Expectation is what one provider must do with one canonical term.
//
// Why is not optional and not decoration. The failure this suite exists to catch is a wire
// form somebody was confident about and nobody had read the documentation for, so an
// expectation that cannot name its source is exactly the thing being guarded against.
type Expectation struct {
	// Wire is a substring the emitted request must contain. Substring rather than the whole
	// request, so a provider adding a $select or a page size does not have to be re-declared
	// everywhere.
	Wire string

	// Refused says the provider must answer with an UnsupportedError instead of sending
	// anything. Refusing is a correct answer; quietly sending a request that cannot honour
	// the term is not.
	Refused bool

	// Why cites the documentation the expectation rests on, or says plainly that it rests on
	// nothing.
	Why string
}

// Emitter runs one search against a provider wired to a stub, and reports what the provider
// put on the wire for it.
//
// The provider supplies this rather than the suite, because "the request" is a URL on three
// of the four providers and an IMAP command on the fourth, and flattening that difference
// here would mean inventing a shape none of them has.
type Emitter func(t *testing.T, q mail.Query) (request string, err error)

// QueryTranslation checks a provider against every canonical term.
//
// A term the provider has no expectation for fails. That is the point of the whole
// arrangement: a new term cannot be added to the canonical list without each provider saying
// what it does with it, and a new provider cannot be written without answering for all of
// them.
func QueryTranslation(t *testing.T, emit Emitter, expects map[string]Expectation) {
	t.Helper()

	for _, term := range QueryTerms {
		t.Run(term.Name, func(t *testing.T) {
			want, declared := expects[term.Name]
			if !declared {
				t.Fatalf("this provider has not said what it sends for %q. Every canonical "+
					"term needs an expectation with a citation; a term nobody has answered "+
					"for is how a translation nobody checked reaches a mailbox", term.Name)
			}
			if strings.TrimSpace(want.Why) == "" {
				t.Fatalf("the expectation for %q cites nothing. An expectation written from "+
					"memory is the bug this suite exists to catch", term.Name)
			}

			request, err := emit(t, term.Query)

			if want.Refused {
				if err == nil {
					t.Fatalf("this term cannot be honoured here and must be refused, not sent. "+
						"The request went out as: %s", request)
				}
				var unsupported *mail.UnsupportedError
				if !errors.As(err, &unsupported) {
					t.Fatalf("want an UnsupportedError so a caller can tell a refusal from a "+
						"failure worth retrying, got %T: %v", err, err)
				}
				if unsupported.Op == "" {
					t.Error("the refusal names no operation, so it reads as though the whole " +
						"capability were missing; the neighbouring searches work")
				}
				return
			}

			if err != nil {
				t.Fatalf("search failed: %v", err)
			}
			if !strings.Contains(request, want.Wire) {
				t.Errorf("the request does not carry this term.\n want: %s\n  got: %s\n%s",
					want.Wire, request, want.Why)
			}
		})
	}

	// An expectation for a term that is not in the canonical list is a stale one, left behind
	// when a term was renamed. Left in place it looks like coverage and provides none.
	t.Run("no expectation names a term that no longer exists", func(t *testing.T) {
		known := make(map[string]bool, len(QueryTerms))
		for _, term := range QueryTerms {
			known[term.Name] = true
		}
		for name := range expects {
			if !known[name] {
				t.Errorf("expectation %q matches no canonical term; it is asserting nothing", name)
			}
		}
	})
}
