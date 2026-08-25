package aggregate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/tfyl/mailroom/internal/mail"
)

// ErrCursorScope is returned when a cursor was issued against a different set of accounts
// than the current call. Continuing would silently page through a different scope than the
// first page did, which is worse than making the caller start over.
var ErrCursorScope = errors.New("cursor was issued for a different set of accounts; start a new search")

var ErrCursorMalformed = errors.New("malformed cursor")

// position records where one account stopped.
//
// Cursor is the provider cursor that produced the page currently being consumed, and Skip is
// how far into that page the last call got. Storing the position *within* a page — rather
// than only the next page's cursor — is what lets a total limit span several mailboxes
// without dropping the items that lost the merge.
type position struct {
	Cursor string `json:"c,omitempty"`
	Skip   int    `json:"s,omitempty"`
	Done   bool   `json:"d,omitempty"`
}

type cursorState struct {
	Accounts map[string]position `json:"a"`
	Scope    string              `json:"f"`
}

// scopeFingerprint identifies the exact set of accounts a cursor was issued for.
func scopeFingerprint(accounts []mail.Account) string {
	ids := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = string(a.ID)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func decodeCursor(cursor string, accounts []mail.Account) (cursorState, error) {
	fp := scopeFingerprint(accounts)
	if cursor == "" {
		return cursorState{Accounts: map[string]position{}, Scope: fp}, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return cursorState{}, ErrCursorMalformed
	}
	var st cursorState
	if err := json.Unmarshal(raw, &st); err != nil {
		return cursorState{}, ErrCursorMalformed
	}
	if st.Scope != fp {
		return cursorState{}, ErrCursorScope
	}
	if st.Accounts == nil {
		st.Accounts = map[string]position{}
	}
	return st, nil
}

func encodeCursor(st cursorState) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
