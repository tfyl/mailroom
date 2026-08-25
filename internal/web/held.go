package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
)

// The page where a grant in `hold` mode is answered.
//
// This is the half of the feature that is a control rather than a suggestion, so it is worth
// being clear about why it is here and not in the client. A tool description can tell an agent
// to ask its human, and a well-behaved agent will; nothing about that is checkable from this
// side, and a mode built only on it would be a setting that describes an intention. MCP's
// elicitation would put the question through the client instead, which sounds better and is
// not — it is negotiated per session on a transport this server deliberately runs stateless,
// and its failure mode is a client saying it cannot ask, which would have to mean either
// refusing everything or proceeding. Proceeding is the opt-out being operated by the party
// under control.
//
// So the question is asked here: behind the operator's own session, on a page an MCP client
// has no route to, about an instruction that has already been authorized and is waiting.

// heldRow is one queued action as the page draws it.
type heldRow struct {
	ID      string
	Summary string
	Kind    string
	// What kind of thing this is, in words, for the badge. Colour never carries meaning on
	// its own here any more than anywhere else in this UI.
	KindLabel string
	// Act completes the button: "Approve and send". The badge above it is a noun and this is
	// a verb, which is why they are two fields rather than one — a button reading "Approve
	// and vacation responder" is the kind of sentence a shared string produces.
	Act     string
	Account string
	Grant   string
	// GrantRevoked marks an action whose grant has since been revoked. It does not stop the
	// action being approved: what is waiting is a message the operator can read, and the
	// queue is their outbox rather than the client's. It is shown because deciding whether
	// to send something an agent composed is a different decision once that agent has been
	// cut off, and the page should not hide which one they are making.
	GrantRevoked bool
	Waiting      string
	At           string
	// Expires says when this one stops being answerable, empty when retention is off. It is
	// on the row rather than only in the prose below because the queue is worked oldest
	// first, and the one at the top is the one closest to going.
	Expires string

	// The parts of a held send worth reading before approving it. Empty for the other kinds,
	// whose summary is the whole of what they do.
	To          []string
	Cc          []string
	Subject     string
	Body        string
	Attachments []string
	// DraftID is set for the one held action whose content is not here: a request to send a
	// draft the client had already saved. The draft is in the mailbox, where its owner can
	// read and edit it before approving, which is why it is named rather than copied.
	DraftID string

	// Resolution and Resolved describe an action that has already been answered.
	Resolution string
	Resolved   string
}

// heldQueue draws what is waiting, and what was recently answered.
func (s *Server) heldQueue(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)

	pending, err := s.holds.Pending(r.Context(), me.ID)
	if err != nil {
		http.Error(w, "could not load the held actions", http.StatusInternalServerError)
		return
	}
	recent, err := s.holds.Recent(r.Context(), me.ID, 20)
	if err != nil {
		http.Error(w, "could not load the held actions", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	ttl := s.holds.TTL()
	rows := make([]heldRow, 0, len(pending))
	for _, a := range pending {
		row := heldViewOf(a, now)
		if ttl > 0 {
			row.Expires = humanUntil(a.CreatedAt.Add(ttl), now)
		}
		rows = append(rows, row)
	}
	done := make([]heldRow, 0, len(recent))
	for _, a := range recent {
		done = append(done, heldViewOf(a, now))
	}

	data := map[string]any{"Pending": rows, "Recent": done}
	// Stated on the page rather than left to be discovered. What waits here is a message
	// that exists nowhere else, and how long it waits is the operator's setting.
	if ttl > 0 {
		data["Retention"] = spanOf(ttl)
	}
	// Matched against what is actually there rather than repeating the query string, so the
	// confirmation names something this server did.
	if msg := r.URL.Query().Get("done"); msg != "" {
		data["Done"] = msg
	}
	if msg := r.URL.Query().Get("failed"); msg != "" {
		data["Failed"] = msg
	}
	s.render(w, r, "held", "Held", "held", data)
}

// approveHeld performs one queued action.
func (s *Server) approveHeld(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)

	action, err := s.holds.Approve(r.Context(), me.ID, r.FormValue("id"))
	if errors.Is(err, held.ErrNotPending) {
		// An id belonging to somebody else, one already answered, and one that never existed
		// all land here and are reported the same way. Telling them apart would confirm that
		// an id is real, and the second of the three is an ordinary double submit.
		http.Error(w, "nothing is waiting under that id", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Warn("a held action could not be carried out",
			"action", action.ID, "kind", action.Kind, "err", err)
		http.Redirect(w, r, "/held?failed="+url.QueryEscape(capitalise(err.Error())+"."),
			http.StatusSeeOther)
		return
	}

	s.log.Info("held action approved", "action", action.ID, "kind", action.Kind,
		"grant", action.GrantID)
	http.Redirect(w, r, "/held?done="+url.QueryEscape("Done — "+action.Summary+"."),
		http.StatusSeeOther)
}

// declineHeld discards one queued action without performing it.
func (s *Server) declineHeld(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)

	action, err := s.holds.Decline(r.Context(), me.ID, r.FormValue("id"))
	if err != nil {
		http.Error(w, "nothing is waiting under that id", http.StatusNotFound)
		return
	}

	s.log.Info("held action discarded", "action", action.ID, "kind", action.Kind,
		"grant", action.GrantID)
	http.Redirect(w, r, "/held?done="+url.QueryEscape("Discarded — "+action.Summary+"."),
		http.StatusSeeOther)
}

// heldKinds names each kind of held action in the words the page uses. A badge reading
// `set_vacation` is the wire format leaking onto a page somebody has to make a decision from.
var heldKinds = map[held.Kind]string{
	held.KindSend:        "send",
	held.KindSendDraft:   "send a draft",
	held.KindTrash:       "delete",
	held.KindModify:      "move to the bin",
	held.KindFilterAdd:   "new filter",
	held.KindFilterDrop:  "delete a filter",
	held.KindSetVacation: "vacation responder",
}

// heldActs is the verb half, for the button. `trash` is not here because it is the one kind
// whose verb is in its payload — trashing and deleting are the same tool and not the same act,
// and a button that said the wrong one of them would be the worst button on this page.
//
// `modify` is not here for a nearby reason: a held label change is queued whole, so the button
// would have to name both the bin and whatever else travelled with it. Its summary already
// says which labels and what applying them does, which is the sentence to decide on.
var heldActs = map[held.Kind]string{
	held.KindSend:        "send",
	held.KindSendDraft:   "send",
	held.KindFilterAdd:   "add it",
	held.KindFilterDrop:  "delete it",
	held.KindSetVacation: "set it",
}

// heldViewOf draws one action, unpacking a send far enough to be read before it is approved.
//
// A held send is the only kind whose summary is not the whole story: "send Re: the invoice to
// finance@example.com" says who and what about, and approving it means agreeing to the words
// in it. So the body is on the page. Everything else — a filter, a delete, an auto-reply — is
// fully described by its own one-line summary, and a second rendering of the same fact would
// be noise.
func heldViewOf(a held.Action, now time.Time) heldRow {
	row := heldRow{
		ID: a.ID, Summary: a.Summary, Kind: string(a.Kind),
		KindLabel: heldKinds[a.Kind], Account: a.Account,
		Grant: a.GrantLabel, GrantRevoked: a.GrantRevoked,
		Waiting: humanSince(a.CreatedAt, now),
		At:      a.CreatedAt.Format("2 Jan 15:04"),
	}
	if row.KindLabel == "" {
		// A kind written by a newer build. Naming it as it was stored is better than an
		// empty badge on a row that has to be decided about.
		row.KindLabel = string(a.Kind)
	}
	row.Act = heldActs[a.Kind]
	if a.Kind == held.KindTrash {
		var payload held.TrashPayload
		if json.Unmarshal(a.Payload, &payload) == nil && payload.Action != "" {
			row.Act = payload.Action + " them"
		}
	}
	if row.Act == "" {
		row.Act = "carry it out"
	}
	if row.Grant == "" {
		row.Grant = "a deleted grant"
	}
	if a.ResolvedAt != nil {
		row.Resolution, row.Resolved = a.Resolution, a.ResolvedAt.Format("2 Jan 15:04")
	}

	if a.Kind != held.KindSend && a.Kind != held.KindSendDraft {
		return row
	}
	// Answered rows have had their payload dropped, which is the point of dropping it: a
	// message that has been sent or discarded is not kept here afterwards.
	var payload held.SendPayload
	if len(a.Payload) == 0 || json.Unmarshal(a.Payload, &payload) != nil {
		return row
	}
	row.DraftID = payload.Draft
	row.To = addressList(payload.Outgoing.To)
	row.Cc = addressList(payload.Outgoing.Cc)
	row.Subject = payload.Outgoing.Subject
	row.Body = bodyPreview(payload.Outgoing.Body)
	for _, att := range payload.Outgoing.Attachments {
		row.Attachments = append(row.Attachments, att.Filename)
	}
	return row
}

func addressList(in []mail.Address) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.String())
	}
	return out
}

// bodyPreviewLimit is how much of a held message the page shows.
//
// Long enough that an ordinary reply is on the page whole, and bounded because this is a
// queue of several messages rather than a reader: a client that composed forty kilobytes of
// quoted thread would otherwise push everything waiting behind it off the screen.
const bodyPreviewLimit = 4000

// bodyPreview prefers the plain text a message carries. The HTML alternative is markup, and
// this page renders text: showing the source of an HTML body to somebody deciding whether to
// send it is worse than showing the text half, which is what the same message says.
func bodyPreview(b mail.Body) string {
	text := b.Text
	if strings.TrimSpace(text) == "" && b.HTML != "" {
		text = "(this message has an HTML body and no plain-text alternative)"
	}
	if len(text) > bodyPreviewLimit {
		return text[:bodyPreviewLimit] + "\n\n… truncated; approve or discard on what is above."
	}
	return text
}
