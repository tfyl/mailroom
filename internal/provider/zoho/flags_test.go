package zoho

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The flag mode is setFlag, and it names the flag rather than numbering it.
//
// It used to send `changeFlag` with a numeric id. Zoho documents eleven modes for
// updatemessage — markAsRead, markAsUnread, moveMessage, setFlag, applyLabel, removeLabel,
// removeAllLabels, archiveMails, unArchiveMails, moveToSpam, markNotSpam — and changeFlag is
// not among them, so nothing was being asked for at all.
func TestStarringUsesTheDocumentedFlagMode(t *testing.T) {
	var bodies []map[string]any
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		bodies = append(bodies, body)
		writeEnvelope(t, w, map[string]any{})
	})

	starred := true
	if err := p.SetFlags(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "10/1"}},
		mmail.FlagUpdate{Starred: &starred}); err != nil {
		t.Fatalf("starring: %v", err)
	}

	if len(bodies) != 1 {
		t.Fatalf("starring is one request, got %d", len(bodies))
	}
	if bodies[0]["mode"] != "setFlag" {
		t.Errorf("mode = %v, want setFlag; changeFlag is not a mode Zoho documents", bodies[0]["mode"])
	}
	if bodies[0]["flagid"] != flagNameFollowUp {
		t.Errorf("flagid = %v, want the name %q: the update endpoint takes the name where the "+
			"listing endpoint takes the number", bodies[0]["flagid"], flagNameFollowUp)
	}
}

// Marking mail read must not send the flag mode at all. Zoho keeps read state and the
// follow-up flag in separate modes, so an update naming one of them has no business writing
// the other.
func TestMarkingReadSendsOnlyTheReadMode(t *testing.T) {
	var modes []string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		modes = append(modes, body.Mode)
		writeEnvelope(t, w, map[string]any{})
	})

	read := true
	if err := p.SetFlags(context.Background(),
		[]mmail.ScopedID{{Account: "acct_1", Native: "10/1"}},
		mmail.FlagUpdate{Read: &read}); err != nil {
		t.Fatalf("marking read: %v", err)
	}

	if len(modes) != 1 || modes[0] != "markAsRead" {
		t.Errorf("marking read should send markAsRead and nothing else, got %v", modes)
	}
}

// Zoho answers with the flag in either shape, and its own samples disagree: the listing
// endpoint's sample carries "flag_not_set" and the search endpoint's carries 2. A reader that
// understood only one of them would report half the mailbox's stars.
func TestTheFollowUpFlagIsReadInEitherShape(t *testing.T) {
	p := &Provider{account: mmail.Account{ID: "acct_1", Alias: "work"}}

	for _, raw := range []string{`3`, `"3"`, `"followup"`} {
		m := p.convert(message{MessageID: "1", FolderID: "10", FlagID: json.RawMessage(raw)})
		if !m.Flags.Starred {
			t.Errorf("flagid %s should read as starred", raw)
		}
	}
	for _, raw := range []string{`0`, `"2"`, `"flag_not_set"`, `"important"`} {
		m := p.convert(message{MessageID: "1", FolderID: "10", FlagID: json.RawMessage(raw)})
		if m.Flags.Starred {
			t.Errorf("flagid %s is not the follow-up flag and must not read as starred", raw)
		}
	}
}
