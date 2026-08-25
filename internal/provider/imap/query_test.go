package imap

import (
	"context"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/conformance"
)

// What this provider is asserted to send for each canonical search, read off the connection
// rather than off the criteria struct it assembled.
//
// IMAP is the provider with the least room to manoeuvre, and the assertions say so. RFC
// 3501's SEARCH keys cover flags, dates, sizes, header fields and text, and that is the whole
// list; there is nothing in it about MIME structure, so "has an attachment" cannot be asked
// at all and is refused rather than dropped.
//
// Free text is the term worth reading twice. It goes out as TEXT, which the RFC defines as
// matching a substring of the message — "Messages that contain the specified string in the
// header or body of the message" — with no operator syntax anywhere in the grammar. A caller
// who writes Gmail's `from:x is:unread` into the query field and sends it here is searching
// for that literal string, and will find nothing while being told the search succeeded.
// Nothing in this file fixes that; it is the reason the tool's own description should not
// promise provider syntax across a fan-out.
func TestIMAPQueryTranslation(t *testing.T) {
	conformance.QueryTranslation(t, emitIMAPSearch, map[string]conformance.Expectation{
		"free text": {
			Wire: `TEXT "sasquatch"`,
			Why: "RFC 3501 6.4.4: TEXT matches messages that contain the specified string in " +
				"the header or body, as a substring and case-insensitively. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		// These three are asked for as FROM, TO and SUBJECT rather than as HEADER, which is
		// not the same question: RFC 3501 defines them against the envelope structure's
		// field, while HEADER matches the raw header line. The client library makes that
		// substitution itself, and reading the criteria struct rather than the connection
		// would not have shown it.
		"sender": {
			Wire: `FROM "hedgehog@example.com"`,
			Why: "RFC 3501 6.4.4: FROM matches messages containing the specified string in " +
				"the envelope structure's FROM field. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"recipient": {
			Wire: `TO "badger@example.com"`,
			Why: "RFC 3501 6.4.4: TO matches the envelope structure's TO field. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"subject": {
			Wire: `SUBJECT "aardvark"`,
			Why: "RFC 3501 6.4.4: SUBJECT matches the envelope structure's SUBJECT field. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"unread": {
			Wire: "UNSEEN",
			Why: "RFC 3501 6.4.4 defines UNSEEN as messages that do not have the \\Seen flag " +
				"set. https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"starred": {
			Wire: "FLAGGED",
			Why: "RFC 3501 6.4.4 defines FLAGGED as messages with the \\Flagged flag set, " +
				"which is the nearest thing IMAP has to a star. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"has attachment": {
			Refused: true,
			Why: "RFC 3501 6.4.4 lists every SEARCH key, and none of them concerns " +
				"attachments or MIME structure — the section goes the other way and permits a " +
				"server to exclude non-text body parts from matching entirely. RFC 9051 adds " +
				"none either, and no registered extension covers it. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"unread alongside free text": {
			Wire: "UNSEEN",
			Why: "SEARCH keys are ANDed by juxtaposition, so a flag key and a text key compose " +
				"in one command with nothing dropped. https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"starred alongside free text": {
			Wire: "FLAGGED",
			Why: "SEARCH keys are ANDed by juxtaposition, as above. " +
				"https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
		"attachment alongside free text": {
			Refused: true,
			Why: "there is no attachment key to compose with; refusing the whole search is the " +
				"only answer that is not a wrong one, since the free-text half alone would " +
				"return mail without attachments. https://www.rfc-editor.org/rfc/rfc3501.txt",
		},
	})
}

// emitIMAPSearch runs one search against the in-memory server and returns the SEARCH command
// the provider sent, taken off the connection.
func emitIMAPSearch(t *testing.T, q mmail.Query) (string, error) {
	t.Helper()

	addr, listener, user, pass := startRecordingServer(t, 1)

	p, err := New(context.Background(), mmail.Account{ID: "acct_imap", Alias: "imap"},
		Config{Host: addr, Username: user, Password: pass})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	_, searchErr := p.Search(context.Background(), q, "")
	return searchLine(listener.commands()), searchErr
}

// searchLine picks the SEARCH command out of the transcript. The rest of it is the login and
// the SELECT, which every search sends and none of these assertions is about.
func searchLine(transcript string) string {
	for _, line := range strings.Split(transcript, "\r\n") {
		if strings.Contains(line, "SEARCH") {
			return line
		}
	}
	return transcript
}
