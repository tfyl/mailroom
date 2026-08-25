package e2e

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// TestClientLifecycle is the walk a real client makes, start to finish, over HTTP.
//
// It exists to prove the harness drives the product rather than a rehearsal of it: dynamic
// registration, the consent screen a human reads, PKCE, the token exchange, MCP initialize
// over Streamable HTTP with the bearer token, tools/list, and a call that changes a mailbox.
// Everything else in this package is a question asked of the machine this sets up.
func TestClientLifecycle(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	c := r.register("Postbox")
	token, id := r.authorize(c, approval{
		label:    "Postbox",
		accounts: []mail.Account{work},
		caps:     []mail.Capability{mail.CapRead, mail.CapSend},
		mode:     grant.ModeUnattended,
	})

	stored, err := r.db.Grant(r.ctx, id)
	if err != nil {
		t.Fatalf("reading back the grant: %v", err)
	}
	if stored.Mode != grant.ModeUnattended {
		t.Fatalf("mode recorded as %q", stored.Mode)
	}
	if !stored.Caps.Has(mail.CapSend) || stored.Caps.Has(mail.CapDestructive) {
		t.Fatalf("capabilities recorded as %v", stored.Caps.Strings())
	}

	s := r.connect(token)
	names := s.toolNames()
	for _, want := range []string{"mail_accounts", "mail_search", "mail_send"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list did not offer %s; it offered %v", want, names)
		}
	}
	for _, notWanted := range []string{"mail_trash", "mail_filters", "mail_settings", "mail_draft"} {
		if slices.Contains(names, notWanted) {
			t.Errorf("tools/list offered %s to a grant that holds neither the capability behind it", notWanted)
		}
	}

	accounts := s.callOK("mail_accounts", map[string]any{})
	if !strings.Contains(accounts.text, "ada@work.example") {
		t.Errorf("mail_accounts did not name the linked mailbox:\n%s", accounts.text)
	}

	sent := s.callOK("mail_send", map[string]any{
		"account": "work",
		"to":      []map[string]any{{"email": "finance@example.net"}},
		"subject": "the invoice",
		"body":    "here it is",
	})
	if sent.payload["sent"] == nil {
		t.Fatalf("mail_send did not report a sent id:\n%s", sent.text)
	}

	messages := r.mailbox(work).sentMessages()
	if len(messages) != 1 {
		t.Fatalf("the mailbox received %d messages, want 1", len(messages))
	}
	if messages[0].Subject != "the invoice" || messages[0].To[0].Email != "finance@example.net" {
		t.Fatalf("the mailbox received %+v", messages[0])
	}

	row := r.lastAuditFor("mail.send")
	if row.Outcome != "ok" || row.Capability != string(mail.CapSend) {
		t.Errorf("audit row was outcome=%q capability=%q", row.Outcome, row.Capability)
	}
	if row.Affected == nil || *row.Affected != 1 {
		t.Errorf("audit row affected = %v, want 1 recipient", row.Affected)
	}
	if len(row.Detail.To) != 1 || row.Detail.To[0] != "finance@example.net" {
		t.Errorf("audit detail recorded recipients %v", row.Detail.To)
	}
	if row.Detail.Subject != "the invoice" {
		t.Errorf("audit detail recorded subject %q", row.Detail.Subject)
	}
	if strings.Contains(row.Detail.Subject+strings.Join(row.Detail.To, " "), "here it is") {
		t.Error("the message body reached the audit log")
	}
}

// TestRevocationStopsTheToken is the property every other check here rests on: the grant is
// re-read per request, so revoking one stops the token that was already issued.
func TestRevocationStopsTheToken(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")

	c := r.register("Postbox")
	token, id := r.authorize(c, approval{
		label: "Postbox", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapRead},
	})

	s := r.connect(token)
	s.callOK("mail_accounts", map[string]any{})

	r.revoke(id)

	// An established session keeps its transport; the next call still has to be refused,
	// because the refusal is per request rather than per connection.
	if err := r.connectExpectingFailure(token); err == nil {
		t.Fatal("a revoked grant's token still completed MCP initialize")
	}
}

// TestAnMCPClientCannotReachTheOperatorSurface.
//
// The held queue is the whole of what makes `hold` a control rather than a suggestion, and it
// works only if the party being controlled cannot answer its own question. An MCP client
// holds a bearer token and nothing else: no operator session, no CSRF seed. This drives it at
// the endpoints that would let it approve its own send, revoke nothing, or read the queue.
func TestAnMCPClientCannotReachTheOperatorSurface(t *testing.T) {
	r := newRig(t, options{})
	work := r.link("work", "ada@work.example")
	s, token, id := r.grantWithToken(approval{
		label: "Held", accounts: []mail.Account{work},
		caps: []mail.Capability{mail.CapSend}, mode: grant.ModeHold,
	})
	s.callOK("mail_send", map[string]any{
		"account": "work", "to": []map[string]any{{"email": "x@example.net"}},
		"subject": "waiting", "body": "x",
	})
	action := r.pending()[0].ID

	// A CSRF token minted for the operator, offered by the client. Holding one is not the
	// same as being signed in, and the guard runs first.
	stolen := r.csrfToken()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	attempt := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(r.ctx, method, r.baseURL+path, body)
		if err != nil {
			t.Fatalf("building %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	for _, probe := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/held", nil},
		{http.MethodPost, "/held/approve", url.Values{"csrf_token": {stolen}, "id": {action}}},
		{http.MethodPost, "/held/decline", url.Values{"csrf_token": {stolen}, "id": {action}}},
		{http.MethodGet, "/audit", nil},
		{http.MethodGet, "/grants", nil},
		{http.MethodPost, "/grants/edit", url.Values{
			"csrf_token": {stolen}, "id": {string(id)}, "mode": {"unattended"},
			"accounts": {string(work.ID)}, "capabilities": {"send"}, "expires_days": {"never"},
		}},
	} {
		status, body := attempt(probe.method, probe.path, probe.form)
		if status == http.StatusOK || status == http.StatusSeeOther {
			t.Errorf("a bearer token reached %s %s: %d\n%s", probe.method, probe.path, status, body)
		}
	}

	if got := len(r.mailbox(work).sentMessages()); got != 0 {
		t.Fatalf("the client got %d of its own held sends approved", got)
	}
	if len(r.pending()) != 1 {
		t.Fatalf("the queue holds %d actions after the client's attempts", len(r.pending()))
	}
	stored, err := r.db.Grant(r.ctx, id)
	if err != nil || stored.Mode != grant.ModeHold {
		t.Fatalf("the grant's mode is now %v (%v)", stored, err)
	}
}
