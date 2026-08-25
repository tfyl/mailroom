package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// accountsPayload is the shape Zoho's account listing answers with, cut down to the fields
// this file reads and populated with invented ids and example.com addresses.
//
// Two accounts, because a Zoho login holds more than one and each carries its own settings.
// The decoy comes first so that a provider taking the head of the list rather than matching
// its own account id reports the wrong mailbox's aliases and the wrong mailbox's auto-reply.
func accountsPayload(vacation []map[string]any) []map[string]any {
	return []map[string]any{
		{
			"accountId":           "8000",
			"primaryEmailAddress": "someone-else@example.com",
			"sendMailDetails": []map[string]any{
				{"fromAddress": "someone-else@example.com", "displayName": "Someone Else", "validated": true},
			},
			"vacationResponse": []map[string]any{
				{"subject": "the wrong mailbox", "content": "this belongs to another account"},
			},
		},
		{
			"accountId":           "9000",
			"primaryEmailAddress": "work@example.com",
			"sendMailDetails": []map[string]any{
				// The mailbox's own address, exactly as Zoho publishes it: validated false,
				// on the address the account is.
				{"fromAddress": "work@example.com", "displayName": "Work", "validated": false},
				{"fromAddress": "verified-alias@example.com", "displayName": "Verified Alias", "validated": true},
				{"fromAddress": "pending-alias@example.com", "displayName": "Pending Alias", "validated": false},
				{"fromAddress": "", "displayName": "no address at all"},
			},
			"vacationResponse": vacation,
		},
	}
}

func settingsProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	return testProvider(t, handler)
}

// accountsHandler answers the listing and fails any other route, so a test that stops
// reaching /accounts is a failure rather than a quiet pass.
func accountsHandler(t *testing.T, vacation []map[string]any, seen *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Method + " " + r.URL.Path
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/accounts" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeEnvelope(t, w, accountsPayload(vacation))
	}
}

// The aliases come from Zoho's send-as list, which is the question mail_settings asks. This
// pins the route as well as the answer: the single-account endpoint is documented without a
// vacationResponse field, so a provider that drifted onto it would read aliases correctly and
// report every auto-reply as absent.
func TestZohoReadsAliasesFromTheAccountListing(t *testing.T) {
	var seen string
	p := settingsProvider(t, accountsHandler(t, nil, &seen))

	aliases, err := p.ListSendAs(context.Background())
	if err != nil {
		t.Fatalf("listing aliases: %v", err)
	}
	if seen != "GET /api/accounts" {
		t.Errorf("aliases were read from %q, want GET /api/accounts", seen)
	}

	// The empty-address row is not an address anybody can send from and must not be offered.
	if len(aliases) != 3 {
		t.Fatalf("got %d aliases, want 3: %+v", len(aliases), aliases)
	}
	for i, want := range []string{"work@example.com", "verified-alias@example.com", "pending-alias@example.com"} {
		if aliases[i].Address != want {
			t.Errorf("alias %d = %q, want %q", i, aliases[i].Address, want)
		}
	}
	if aliases[0].DisplayName != "Work" {
		t.Errorf("display name = %q, want Work", aliases[0].DisplayName)
	}
	// Zoho publishes no reply-to field on the account record, so reporting one would mean
	// inventing the name of a field to read it out of.
	if aliases[0].ReplyTo != "" {
		t.Errorf("reply-to = %q, but Zoho publishes no field to read one from", aliases[0].ReplyTo)
	}
}

// Verification is the assertion that costs somebody a failed send if it is wrong in either
// direction, so both directions are pinned here.
//
// Zoho's own published sample carries validated:false on the mailbox's own address, so
// reading that field alone reports the address the account *is* as unusable. Every other row
// is verified only where Zoho said so.
func TestZohoMarksOnlyTheMailboxAndValidatedAliasesVerified(t *testing.T) {
	p := settingsProvider(t, accountsHandler(t, nil, nil))

	aliases, err := p.ListSendAs(context.Background())
	if err != nil {
		t.Fatalf("listing aliases: %v", err)
	}

	byAddress := map[string]mmail.SendAs{}
	for _, a := range aliases {
		byAddress[a.Address] = a
	}

	mailbox := byAddress["work@example.com"]
	if !mailbox.Verified {
		t.Error("the mailbox's own address must be verified; Zoho reports validated:false on it, " +
			"and a caller that filtered on Verified could not send at all")
	}
	if !mailbox.Primary || !mailbox.Default {
		t.Errorf("the address matching primaryEmailAddress must be the primary and the default: %+v", mailbox)
	}

	if verified := byAddress["verified-alias@example.com"]; !verified.Verified {
		t.Error("an alias Zoho reports as validated must be reported verified")
	} else if verified.Primary || verified.Default {
		t.Errorf("only the mailbox's own address is primary: %+v", verified)
	}

	if pending := byAddress["pending-alias@example.com"]; pending.Verified {
		t.Error("an alias Zoho does not report as validated must stay unverified; reporting it " +
			"verified is a send that is accepted here and fails later")
	}
}

// A login holds several accounts. Picking the wrong one reports another mailbox's settings
// under this mailbox's name, which no error would ever surface.
func TestZohoReadsTheAccountItWasBuiltFor(t *testing.T) {
	p := settingsProvider(t, accountsHandler(t, []map[string]any{
		{"subject": "away", "content": "back next week"},
	}, nil))

	aliases, err := p.ListSendAs(context.Background())
	if err != nil {
		t.Fatalf("listing aliases: %v", err)
	}
	for _, a := range aliases {
		if strings.Contains(a.Address, "someone-else") {
			t.Fatalf("read another account's aliases: %+v", aliases)
		}
	}

	vacation, err := p.GetVacation(context.Background())
	if err != nil {
		t.Fatalf("reading the vacation reply: %v", err)
	}
	if vacation.Subject != "away" {
		t.Errorf("subject = %q, want the vacation reply of account 9000", vacation.Subject)
	}
}

// A listing that does not hold this mailbox is a failure rather than an empty answer: an
// account that is not there cannot be reported as one with no aliases and no auto-reply.
func TestZohoRefusesAListingWithoutItsOwnAccount(t *testing.T) {
	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, []map[string]any{{"accountId": "8000", "primaryEmailAddress": "someone-else@example.com"}})
	})

	if _, err := p.ListSendAs(context.Background()); err == nil {
		t.Error("a listing that does not contain this mailbox must be an error, not an empty alias list")
	}
	if _, err := p.GetVacation(context.Background()); err == nil {
		t.Error("a listing that does not contain this mailbox must be an error, not an auto-reply reported as off")
	}
}

// Zoho spells ids as strings on some endpoints and bare numbers on others, and nothing says
// which the account listing uses. Matching on only one spelling would report this mailbox as
// missing from its own account list.
func TestZohoMatchesAnUnquotedAccountID(t *testing.T) {
	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":{"code":200,"description":"success"},"data":[
			{"accountId":9000,"primaryEmailAddress":"work@example.com",
			 "sendMailDetails":[{"fromAddress":"work@example.com","displayName":"Work","validated":false}]}
		]}`))
	})

	aliases, err := p.ListSendAs(context.Background())
	if err != nil {
		t.Fatalf("an unquoted account id was not matched: %v", err)
	}
	if len(aliases) != 1 || aliases[0].Address != "work@example.com" {
		t.Errorf("got %+v, want the mailbox's own address", aliases)
	}
}

// Zoho stores an auto-reply with no enabled flag anywhere in it, so presence is the whole
// signal. A stored response reads as enabled and no stored response reads as off.
func TestZohoReportsAStoredVacationReplyAsEnabled(t *testing.T) {
	p := settingsProvider(t, accountsHandler(t, []map[string]any{{
		"subject": "Away on leave",
		"content": "Back on the 12th.",
		// The audience Zoho reads back is a bare integer whose mapping to the names it
		// accepts on write is documented nowhere, and the dates contradict the format the
		// write API defines. Both are present here and neither may be read.
		"sendTo":       0,
		"fromDate":     "05/04/2024",
		"toDate":       "19/05/2024",
		"infiniteDate": false,
		"vacationId":   "1400000000000000001",
	}}, nil))

	v, err := p.GetVacation(context.Background())
	if err != nil {
		t.Fatalf("reading the vacation reply: %v", err)
	}
	if !v.Enabled {
		t.Error("a stored vacation reply must read as enabled; Zoho carries no flag saying so, " +
			"and reporting it off would tell somebody an auto-reply they still have is silent")
	}
	if v.Subject != "Away on leave" || v.Body != "Back on the 12th." {
		t.Errorf("the stored reply did not survive the read: %+v", v)
	}
	// sendTo is an integer here and a name on write, and nothing documents the correspondence.
	// Reporting a restriction from it would be a guess about who receives somebody's mail.
	if v.RestrictToContacts || v.RestrictToDomain {
		t.Errorf("restrictions were reported from an undocumented integer: %+v", v)
	}
}

func TestZohoReportsNoVacationReplyWhenNoneIsStored(t *testing.T) {
	p := settingsProvider(t, accountsHandler(t, nil, nil))

	v, err := p.GetVacation(context.Background())
	if err != nil {
		t.Fatalf("reading the vacation reply: %v", err)
	}
	if v.Enabled || v.Subject != "" || v.Body != "" {
		t.Errorf("an account with no stored reply must read as off and empty: %+v", v)
	}
}

// Switching off is the one write Zoho can be asked for exactly, so this pins the whole
// request: the mode is the entire instruction, and the path names the account.
//
// The responder has to be on for the write to happen at all. Deleting one that is not there
// is a 500 at Zoho's end rather than a no-op, so the switch-off reads first and does nothing
// when there is nothing to do — which is what the sibling test covers.
func TestZohoSwitchesTheVacationReplyOff(t *testing.T) {
	var method, path string
	var body map[string]any

	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			writeEnvelope(t, w, accountsPayload([]map[string]any{
				{"subject": "away", "content": "back next week"},
			}))
			return
		}
		method, path = r.Method, r.URL.Path
		body = decodeBody(t, r)
		writeEnvelope(t, w, nil)
	})

	if err := p.SetVacation(context.Background(), mmail.Vacation{Enabled: false}); err != nil {
		t.Fatalf("switching the vacation reply off: %v", err)
	}
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	if path != "/api/accounts/9000" {
		t.Errorf("path = %q, want /api/accounts/9000", path)
	}
	if body["mode"] != "deleteVacationReply" {
		t.Errorf("mode = %v, want deleteVacationReply — it is the entire instruction", body["mode"])
	}
}

// Zoho can answer HTTP 200 with a failing envelope inside it. A switch-off read as a success
// would leave the auto-reply running and report it stopped, so the envelope has to be
// inspected — which do only does when it is given somewhere to decode into.
func TestZohoReportsAFailedSwitchOffFromTheEnvelope(t *testing.T) {
	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":{"code":400,"description":"Invalid Input"},"data":{"errorCode":"INVALID_MODE"}}`))
	})

	if err := p.SetVacation(context.Background(), mmail.Vacation{Enabled: false}); err == nil {
		t.Error("a failing envelope inside an HTTP 200 must be reported; the auto-reply is still running")
	}
}

// Switching on is refused rather than approximated. Zoho needs a start date, an end date and
// a sending interval, and mailroom's model carries none of them — so any request mailroom
// sent would be deciding when somebody's auto-reply stops.
func TestZohoRefusesToSwitchAVacationReplyOn(t *testing.T) {
	var reached bool
	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		writeEnvelope(t, w, nil)
	})

	err := p.SetVacation(context.Background(), mmail.Vacation{
		Enabled: true, Subject: "Away", Body: "Back on the 12th.",
	})
	if err == nil {
		t.Fatal("switching a vacation reply on must be refused, not sent with invented dates")
	}
	if reached {
		t.Error("the refusal must happen before Zoho is asked to do anything")
	}

	// The refusal has to travel as a type. Formatted into a message it arrives as a generic
	// error, which is the code that means "worth retrying" — the opposite of what this is.
	var unsupported *mmail.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("refusal was %T, want *mail.UnsupportedError", err)
	}
	if unsupported.Op == "" {
		t.Error("the refusal must name the operation; naming the capability would tell a caller " +
			"to stop reading the aliases and the auto-reply, which both work")
	}
	if unsupported.Capability != mmail.CapSettings {
		t.Errorf("capability = %q, want %q", unsupported.Capability, mmail.CapSettings)
	}
}

// The capability is claimed from the interface, so this is the assertion that the tool layer's
// type check will now find a Zoho mailbox behind mail_settings at all.
func TestZohoSatisfiesTheSettingsInterface(t *testing.T) {
	var p any = &Provider{}
	if _, ok := p.(mmail.SettingsManager); !ok {
		t.Fatal("zoho must satisfy mail.SettingsManager or mail_settings cannot reach it")
	}
	if _, ok := p.(mmail.ForwardingReader); ok {
		t.Error("forwarding is not implemented here; claiming it would offer an empty answer as a real one")
	}
	if _, ok := p.(mmail.DelegateManager); ok {
		t.Error("delegation is not implemented here; claiming it would offer an empty answer as a real one")
	}
}

// Guards the decode rather than the call: Zoho's account record carries about fifty fields
// and mailroom reads four, so an unrelated one arriving must not disturb the ones it needs.
func TestZohoIgnoresTheRestOfTheAccountRecord(t *testing.T) {
	payload := accountsPayload(nil)
	payload[1]["policyId"] = map[string]any{"zoid": 3226386}
	payload[1]["extraStorage"] = map[string]any{}
	payload[1]["emailAddress"] = []map[string]any{
		// The receiving side. These are addresses the mailbox collects mail at, which is not
		// the same list as the addresses it may send from, and must not reach ListSendAs.
		{"mailId": "receives-only@example.com", "isAlias": true, "isPrimary": false, "isConfirmed": true},
	}

	p := settingsProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, payload)
	})

	aliases, err := p.ListSendAs(context.Background())
	if err != nil {
		t.Fatalf("listing aliases: %v", err)
	}
	for _, a := range aliases {
		if a.Address == "receives-only@example.com" {
			t.Error("a receiving alias was reported as an address to send from")
		}
	}
	if len(aliases) != 3 {
		t.Errorf("got %d aliases, want 3: %+v", len(aliases), aliases)
	}
}

// The scope list is what a mailbox is linked with, and a scope that is missing at link time
// cannot be added afterwards without the owner consenting again.
func TestZohoAsksForTheScopeTheVacationWriteNeeds(t *testing.T) {
	var read, update bool
	for _, s := range Scopes {
		switch s {
		case "ZohoMail.accounts.READ":
			read = true
		case "ZohoMail.accounts.UPDATE":
			update = true
		}
	}
	if !read {
		t.Error("reading the account record needs ZohoMail.accounts.READ")
	}
	if !update {
		t.Error("switching the vacation reply off needs ZohoMail.accounts.UPDATE; without it the " +
			"write is refused by Zoho and the mailbox has to be linked again to get it")
	}
}

// A guard on the fixtures rather than on the provider: this repository is public, so nothing
// here may carry a real address or a real id.
func TestZohoSettingsFixturesAreSynthetic(t *testing.T) {
	encoded, err := json.Marshal(accountsPayload([]map[string]any{{"subject": "away", "content": "back soon"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range extractAddresses(string(encoded)) {
		if !strings.HasSuffix(address, "@example.com") {
			t.Errorf("fixture carries the non-synthetic address %q", address)
		}
	}
}

func extractAddresses(s string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '"' || r == ',' || r == '{' || r == '}' || r == '[' || r == ']' || r == ' '
	}) {
		if strings.Contains(field, "@") {
			out = append(out, field)
		}
	}
	return out
}

// Deleting a vacation reply that is not there is a 500 at Zoho's end, not a no-op — measured
// against a live mailbox with no responder set. That is the common case by a distance, so
// without this guard the only write this provider supports failed almost every time it was
// called, with an error that read like Zoho being broken.
func TestSwitchingOffAResponderThatIsAlreadyOffAsksZohoNothing(t *testing.T) {
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"code":500,"description":"Internal Error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// No responder set, which is the ordinary state of a mailbox and the one that used to
		// make the switch-off fail.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"code": 200},
			"data":   accountsPayload(nil),
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "9000",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "work@example.com"},
	}

	if err := p.SetVacation(context.Background(), mmail.Vacation{Enabled: false}); err != nil {
		t.Fatalf("switching off something already off is a satisfied request, got: %v", err)
	}
	if writes != 0 {
		t.Errorf("Zoho was asked to delete a responder that does not exist (%d write(s))", writes)
	}
}

// The direction that matters: a read that fails must not be taken as "already off", because
// that reports success for a responder still running.
func TestAFailedReadDoesNotReportTheResponderSwitchedOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":{"code":500,"description":"Internal Error"}}`))
	}))
	t.Cleanup(srv.Close)

	p := &Provider{
		http: srv.Client(), base: srv.URL, accountID: "acct",
		account: mmail.Account{ID: "acct_1", Alias: "work", Address: "you@example.com"},
	}

	if err := p.SetVacation(context.Background(), mmail.Vacation{Enabled: false}); err == nil {
		t.Fatal("a failed read must not be reported as a successful switch-off")
	}
}
