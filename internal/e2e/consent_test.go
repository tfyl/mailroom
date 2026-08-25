package e2e

import (
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// The consent screen, read against what the grants it mints can actually do.
//
// This is the one page in the product whose only job is helping a person decide, and four of
// the five features that landed changed what there is to decide. These drive the real page
// and compare its words to what the same rig then observes at the mailbox.

// consentText renders the consent screen and flattens it to one line, so a claim that wraps
// across three lines of template can still be matched as the sentence a person reads.
func consentText(r *rig) string {
	r.t.Helper()
	c := r.register("Consent reader")
	return strings.Join(strings.Fields(r.consentPage(c)), " ")
}

// TestAFiltersGrantCannotForwardMailOffBox.
//
// This was a finding, recorded here as a failing-when-fixed test: a grant holding `filters`
// alone could create a rule that forwarded every message matching a query to an outside
// address, and `filters` sits in the consent screen's Ordinary group described only as
// "Create and delete filters and rules".
//
// The product already knew what that was. internal/grant/audit.go records a filter's
// forwarding address beside the recipients of a send, "because it is the same act with a
// delay on it … the exfiltration this whole product is trying not to enable", and
// handleSettings refuses to write forwarding at all because it "hands somebody else access to
// the mail itself, which is a decision for a person at a settings page". One tool refused it
// and the other accepted it.
//
// mail_filters now refuses it too, which is what this asserts: the rule is not created, the
// mailbox is untouched, and the refusal says where forwarding is actually set.
func TestAFiltersGrantCannotForwardMailOffBox(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Filer", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapFilters},
	})

	created := s.call("mail_filters", map[string]any{
		"account": "work", "action": "create",
		"query": "has:attachment", "forward": "collector@stranger.example",
	})
	if !created.isError {
		t.Fatalf("a forwarding filter was created:\n%s", created.text)
	}
	if !strings.Contains(created.text, "collector@stranger.example") {
		t.Errorf("the refusal should name the address it declined to forward to:\n%s", created.text)
	}

	// The half that matters: nothing reached the mailbox.
	filters, err := r.mailbox(work).ListFilters(r.ctx)
	if err != nil {
		t.Fatalf("listing filters: %v", err)
	}
	if len(filters) != 0 {
		t.Fatalf("the mailbox holds a rule that should never have been created: %+v", filters)
	}

	// An ordinary rule still works, or the refusal would have taken rule management with it.
	ok := s.callOK("mail_filters", map[string]any{
		"account": "work", "action": "create", "query": "from:newsletter@example.com",
	})
	if ok.isError {
		t.Fatalf("an ordinary filter should still be created:\n%s", ok.text)
	}
}

// TestTheConsentScreenSaysNothingAboutBytesOnDisk.
//
// Ticking `attachments` means more than "download attachment contents": a copy of the file is
// written to this server's own filesystem, and what the client is handed is a URL that is
// itself the credential — no bearer token, fetchable by anyone it is passed to, alive for the
// configured TTL. Every one of those facts is written down for developers, for the model, and
// in docs/. None of them is on the page where the decision is made.
func TestTheConsentScreenSaysNothingAboutBytesOnDisk(t *testing.T) {
	r := newRig(t, options{})
	r.link("work", "ada@work.example")
	page := consentText(r)

	if !strings.Contains(page, `value="attachments"`) {
		t.Fatal("the consent screen does not offer `attachments` at all")
	}
	// Words chosen to be specific rather than suggestive: "link" appears in the stylesheet
	// tag on every page in this UI, and matching that would make the assertion about nothing.
	var mentioned []string
	for _, phrase := range []string{"signed", "url", "disk", "on this server"} {
		if strings.Contains(strings.ToLower(page), phrase) {
			mentioned = append(mentioned, phrase)
		}
	}
	if len(mentioned) > 0 {
		t.Fatalf("the consent screen now mentions %v; re-read this finding", mentioned)
	}
	t.Log("FINDING (medium): the consent screen offers `attachments` without saying that " +
		"granting it writes copies of mail to this server's disk, or that the client is " +
		"handed a token-free signed URL anyone can fetch. docs/grants.md, docs/security.md " +
		"and mail_get_attachment's own description all say so; the page the operator decides " +
		"on says neither.")
}

// TestTheHoldModeCopyOverstatesWhatIsHeld.
//
// The mode card's summary for `hold` reads "Sending, deleting, filters and the vacation
// responder are not carried out when the client asks for them." Two of the things a reader
// will take "deleting" to cover are not held: discarding a draft, and untrashing. The first
// matters because `discard` is a checkbox two cards higher up the same page, so the operator
// has just been thinking about draft deletion in those words.
func TestTheHoldModeCopyOverstatesWhatIsHeld(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	page := consentText(r)
	const claim = "Sending, deleting, filters and the vacation responder are not carried out"
	if !strings.Contains(page, claim) {
		t.Fatalf("the consent screen no longer says %q; this finding is stale", claim)
	}
	for _, exemption := range []string{"untrash", "Untrash"} {
		if strings.Contains(page, exemption) {
			t.Fatalf("the consent screen now names the %s exemption; this finding is stale", exemption)
		}
	}

	// And the behaviour the sentence covers over.
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapDraft, mail.CapDiscard, mail.CapDestructive},
		mode: grant.ModeHold,
	})
	draftID := str(s.callOK("mail_draft", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}}, "subject": "d",
	}).payload["draft_id"])
	s.callOK("mail_draft", map[string]any{"action": "delete", "draft_id": draftID})
	if r.mailbox(work).draftCount() != 0 || len(r.pending()) != 0 {
		t.Fatal("the discard was held after all; this finding is stale")
	}
	t.Logf("FINDING (medium): the consent screen's summary of `hold` says %q. Deleting a "+
		"draft is not held — internal/mcp/tools_write.go's handleDraft has no mode check at "+
		"all, and mail_draft's own steering tells the model \"Nothing on this tool is held, "+
		"deleting included\". Nor is untrash. The operator is told a stricter rule than the "+
		"one the server enforces, on the page where they choose `discard`.", claim)
}

// TestTheAuditPageMarksAHeldOutcome is the regression test for a fix in this commit: the
// auditRow.Held flag was computed and never rendered, so a privileged action still waiting
// for somebody was drawn exactly like one that had gone through.
func TestTheAuditPageMarksAHeldOutcome(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, _ := r.grantFor(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "waiting", "body": "x",
	})

	page, _ := r.page("/audit")
	if !strings.Contains(page, "held") {
		t.Fatal("the audit page does not show the held outcome at all")
	}
	// The badge, not the bare word: the outcome column marks a refusal and marks a change,
	// and a held action is neither of those and still needs answering.
	if !strings.Contains(page, `<span class="badge mono" data-variant="secondary">held</span>`) {
		t.Errorf("a held outcome is drawn as plain text, the same as ok:\n%s",
			outcomeColumn(page))
	}
}

// outcomeColumn trims a rendered audit page down to the part this assertion is about, so a
// failure prints something a person can read.
func outcomeColumn(page string) string {
	i := strings.Index(page, "held")
	if i < 0 {
		return page
	}
	start := max(i-400, 0)
	return page[start:min(i+200, len(page))]
}
