package zoho

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// message is Zoho's listing shape.
//
// The three identifiers are flexString rather than string because Zoho does not spell them
// the same way on every endpoint; see the type's own comment for what was observed where.
type message struct {
	MessageID    flexString      `json:"messageId"`
	FolderID     flexString      `json:"folderId"`
	ThreadID     flexString      `json:"threadId"`
	Subject      string          `json:"subject"`
	Sender       string          `json:"sender"`
	FromAddress  string          `json:"fromAddress"`
	ToAddress    string          `json:"toAddress"`
	CCAddress    string          `json:"ccAddress"`
	Summary      string          `json:"summary"`
	ReceivedTime json.RawMessage `json:"receivedTime"`
	SentDate     json.RawMessage `json:"sentDateInGMT"`
	// Zoho encodes read state as an integer rather than a flag: 0 is unread.
	Status      json.RawMessage `json:"status"`
	FlagID      json.RawMessage `json:"flagid"`
	HasAttach   json.RawMessage `json:"hasAttachment"`
	Size        json.RawMessage `json:"size"`
	Priority    json.RawMessage `json:"priority"`
	ThreadCount json.RawMessage `json:"threadCount"`
}

const defaultPageSize = 50

// unsearchable refuses one filter that cannot ride alongside search terms, naming the filter
// rather than the capability: reading works, searching works, and it is one combination of
// the two that Zoho has no way to express.
func (p *Provider) unsearchable(op, reason string) error {
	return &mmail.UnsupportedError{
		Provider:   mmail.ProviderZoho,
		Account:    p.account.Alias,
		Address:    p.account.Address,
		Capability: mmail.CapRead,
		Op:         op + " alongside search terms",
		Reason:     reason,
	}
}

// Search lists messages, translating the canonical query into Zoho's parameters.
//
// Zoho pages by offset rather than by opaque cursor, so the cursor here carries the next
// start index. The aggregator never looks inside it, which is what lets one provider page by
// token and another by offset without the tool layer knowing.
func (p *Provider) Search(ctx context.Context, q mmail.Query, cursor string) (mmail.Page[mmail.Message], error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = defaultPageSize
	}

	start, err := offsetCursor(cursor)
	if err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	query := url.Values{}
	query.Set("start", strconv.Itoa(start))
	query.Set("limit", strconv.Itoa(limit))
	query.Set("includeto", "true")

	path := "/accounts/" + p.accountID + "/messages/view"

	// Zoho splits browsing from searching. Anything with search terms has to go to the
	// search endpoint, which takes a single structured expression.
	term := searchExpression(q)
	if term != "" {
		path = "/accounts/" + p.accountID + "/messages/search"
		query.Set("searchKey", term)
	}

	// The two endpoints take different parameters, and only one of them takes the filters
	// below. Zoho documents status, attachedMails, labelid and flagid on the listing endpoint
	// and on nothing else; the search endpoint's parameter list is searchKey, receivedTime,
	// start, limit and includeto, and no more.
	//
	// A parameter the search endpoint does not know is not an error there — it is a parameter
	// that does nothing. The whole mailbox comes back, matching the search terms and none of
	// the filter, and the caller reads a full unfiltered page as the answer to a narrow
	// question. Three of these four were being sent to the search endpoint regardless.
	//
	// So each one is either served on the listing endpoint or refused by name, which leaves
	// the caller a query it can actually run. Two of them have documented searchKey
	// equivalents — `has:attachment`, and `label:` by name rather than by id — and wiring
	// those in is worth doing; it is a change to the search expression rather than to which
	// endpoint answers, and it wants the syntax settled first.
	if q.Unread {
		if term != "" {
			return mmail.Page[mmail.Message]{}, p.unsearchable("an unread filter",
				"Zoho's search syntax has no condition for read state at all, and the status "+
					"parameter is only read by the listing endpoint")
		}
		query.Set("status", "unread")
	}
	if q.HasAttach {
		if term != "" {
			return mmail.Page[mmail.Message]{}, p.unsearchable("an attachment filter",
				"the attachedMails parameter is only read by the listing endpoint; Zoho's "+
					"search syntax expresses this as has:attachment, which is not wired up yet")
		}
		query.Set("attachedMails", "true")
	}
	if q.Label != "" {
		if term != "" {
			return mmail.Page[mmail.Message]{}, p.unsearchable("a label filter",
				"the labelid parameter is only read by the listing endpoint, and Zoho's search "+
					"syntax names a label by its display name rather than by the id this "+
					"label carries")
		}
		query.Set("labelid", string(q.Label))
	}
	if q.Starred {
		// The listing endpoint filters on the follow-up flag exactly, with flagid. The search
		// endpoint cannot: its syntax offers only `has:flags`, which matches info and
		// important as well, so a starred filter there would answer with mail flagged some
		// other way and call it starred.
		if term != "" {
			return mmail.Page[mmail.Message]{}, p.unsearchable("a starred filter",
				"Zoho's search syntax can ask only for flagged mail, not for the follow-up "+
					"flag that starred maps to; search for starred mail on its own, or narrow "+
					"these results yourself")
		}
		query.Set("flagid", strconv.Itoa(flagFollowUp))
	}

	var raw []message
	if err := p.get(ctx, path, query, &raw); err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	items := make([]mmail.Message, 0, len(raw))
	for _, m := range raw {
		items = append(items, p.convert(m))
	}

	page := mmail.Page[mmail.Message]{Items: items}
	// A full page implies there may be more. Zoho reports no total, so the only honest
	// signal is a short page meaning the end.
	if len(raw) == limit {
		page.Cursor = strconv.Itoa(start + limit)
	}
	return page, nil
}

// offsetCursor reads the position a listing resumes from.
//
// Zoho's start parameter is 1-indexed rather than 0, and the search endpoint and the folder
// listing take it the same way, so the rule is read in one place: an off-by-one kept in two
// would show up as a mailbox that quietly skips its first message on one of the two paths.
func offsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 1, nil
	}
	parsed, err := strconv.Atoi(cursor)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("malformed cursor %q", cursor)
	}
	return parsed, nil
}

// searchExpression renders the canonical query as Zoho's search syntax.
//
// Zoho's syntax is `field:value`, conditions joined by `::`. Not `field:contains:value`
// joined by `&&`, which is what this used to send — a form Zoho does not parse, so a search
// came back successful and empty rather than refused. Free text is the case that mattered
// most: it was sent as a bare word with no field at all, so every plain-language search
// against a Zoho mailbox found nothing while reporting ok.
//
// Dates are their own shape again: Zoho takes fromDate/toDate as DD-MMM-YYYY, not an epoch
// on a comparison operator. The bound is therefore a whole day rather than an instant, which
// is wider than the caller asked for — a mail search is not worse for returning the rest of
// the day, and refusing the filter outright would be.
func searchExpression(q mmail.Query) string {
	var parts []string
	if q.Raw != "" {
		parts = append(parts, "entire:"+searchTerm(q.Raw))
	}
	if q.From != "" {
		parts = append(parts, "sender:"+searchTerm(q.From))
	}
	if q.To != "" {
		parts = append(parts, "to:"+searchTerm(q.To))
	}
	if q.Subject != "" {
		parts = append(parts, "subject:"+searchTerm(q.Subject))
	}
	if !q.After.IsZero() {
		parts = append(parts, "fromDate:"+q.After.Format(zohoDate))
	}
	if !q.Before.IsZero() {
		parts = append(parts, "toDate:"+q.Before.Format(zohoDate))
	}
	return strings.Join(parts, "::")
}

// zohoDate is the DD-MMM-YYYY that fromDate and toDate are documented to take.
// searchTerm renders a term so Zoho reads it as text rather than as syntax.
//
// A leading hyphen is the case that matters, because it does not fail — it inverts. Asked for
// a subject beginning with one, Zoho answered with the whole mailbox and called it a match:
// measured on the live mailbox, a nonsense term returned nothing and the same term with a
// hyphen in front returned every message on the page, as did a hyphenated word that genuinely
// appears in the mailbox.
//
// Double quotes are what Zoho accepts, and the positive control is the part worth keeping:
// quoting a term that genuinely matches still returns the same twenty results it did bare, so
// this neutralises the inversion rather than trading it for a search that matches nothing.
//
// Terms are otherwise left bare, matching the same rule on the Gmail side. A term containing
// Zoho's own :: separator is not quoted here: unquoted it is refused outright with an error,
// which is a poor message but an honest answer, and no caller is misled by it.
func searchTerm(s string) string {
	if strings.ContainsAny(s, " \t\"") || strings.HasPrefix(s, "-") {
		return `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	return s
}

const zohoDate = "02-Jan-2006"

// Get fetches one message: its metadata from /details, its body from /content.
//
// Two requests, because the content endpoint answers with two fields and no more. The live
// mailbox returns `{"content":"…","messageId":1234567890123456789}` and nothing else, so
// reading only that produced a message with a body and no sender, subject or date — which
// mail_get renders as an empty envelope around some text, dated year 1. The details endpoint
// answers the same shape as the listing, so the message comes back whole.
func (p *Provider) Get(ctx context.Context, id mmail.ScopedID) (mmail.Message, error) {
	folderID, messageID, err := splitNative(id.Native)
	if err != nil {
		return mmail.Message{}, err
	}

	base := fmt.Sprintf("/accounts/%s/folders/%s/messages/%s", p.accountID, folderID, messageID)

	var meta message
	if err := p.get(ctx, base+"/details", nil, &meta); err != nil {
		return mmail.Message{}, err
	}

	var detail struct {
		Content string `json:"content"`
	}
	if err := p.get(ctx, base+"/content", nil, &detail); err != nil {
		return mmail.Message{}, err
	}

	// Neither endpoint reliably echoes the folder, so keep the identifiers the caller already
	// supplied rather than trusting a partial response.
	meta.MessageID = flexString(messageID)
	meta.FolderID = flexString(folderID)

	out := p.convert(meta)
	if strings.Contains(strings.ToLower(detail.Content), "<html") {
		out.Body.HTML = detail.Content
	} else {
		out.Body.Text = detail.Content
	}

	// The manifest is a third request, made only when the message says it has one. Without
	// it the attachments capability is unreachable rather than merely awkward: GetAttachment
	// takes an attachment id, and this is the only place an id is ever produced.
	//
	// A failure here does not fail the read. The message is in hand and a caller asking for
	// a message wants the message; losing the manifest costs the attachment, not the mail.
	if n, ok := asInt(meta.HasAttach); ok && n > 0 {
		if refs, err := p.attachmentRefs(ctx, folderID, messageID); err == nil {
			out.Attachments = refs
		}
	}
	return out, nil
}

// attachmentRefs reads the manifest for one message.
//
// Zoho answers this endpoint in two shapes depending on the account, so both are accepted: a
// bare array of attachments, and an object wrapping one under "attachments". Decoding into a
// RawMessage first and trying each is less clever than picking one, and it is the difference
// between a manifest and silence on an account that answers the other way.
func (p *Provider) attachmentRefs(ctx context.Context, folderID, messageID string) ([]mmail.AttachmentRef, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("/accounts/%s/folders/%s/messages/%s/attachmentinfo",
		p.accountID, folderID, messageID)
	if err := p.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}

	// Field names measured against the live mailbox, which answers:
	//
	//	{"attachments":[{"attachmentSize":697,"attachmentName":"…","attachmentId":"…"}],…}
	//
	// There is no content type in it. Zoho reports one on the download itself, so the
	// manifest carries the name and the size and GetAttachment fills in the rest.
	type wireAttachment struct {
		AttachmentID flexString `json:"attachmentId"`
		Name         string     `json:"attachmentName"`
		Size         flexString `json:"attachmentSize"`
	}

	var list []wireAttachment
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapper struct {
			Attachments []wireAttachment `json:"attachments"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("decoding the attachment manifest: %w", err)
		}
		list = wrapper.Attachments
	}

	refs := make([]mmail.AttachmentRef, 0, len(list))
	for _, a := range list {
		id := a.AttachmentID.String()
		if id == "" {
			// An entry with no id addresses nothing, and reporting it would offer a caller a
			// download that cannot be made.
			continue
		}
		ref := mmail.AttachmentRef{ID: id, Filename: a.Name}
		if n, err := strconv.ParseInt(a.Size.String(), 10, 64); err == nil {
			ref.Size = n
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return refs, nil
}

// GetThread returns the conversation a message belongs to, as far as Zoho will report it.
//
// This used to return an empty thread for most messages, which reads as "there is no
// conversation here" and is the worst of the available answers. Why it was empty is the
// whole of what is worth knowing about Zoho threading:
//
// Zoho threads mail, and /messages/view?threadId=<thread> does return a thread's members —
// on the live mailbox that call answers with all three messages of a three-message
// conversation. But nothing tells mailroom what <thread> is for a message it has listed. The
// listing and the search endpoint answer without a threadId field; Zoho reports one only
// under threadedMails=true, which is a filter that hides mail rather than an annotation, and
// which /messages/search rejects. See the Quirks comment for the measurements.
//
// The id mailroom holds is therefore the message's own, which Zoho accepts as a thread id and
// answers for only when that message started the thread. Asked for a reply's own id, Zoho
// returns an empty array — and that empty array used to be the whole answer.
//
// So the thread is anchored on the message it was reached from, fetched directly, and
// whatever Zoho reports for the guessed thread id is merged in around it. The result always
// contains at least the message asked about, and Derived says the grouping was inferred:
// a caller must not read a one-message answer here as proof there were no replies.
func (p *Provider) GetThread(ctx context.Context, id mmail.ScopedID) (mmail.Thread, error) {
	folderID, threadID, err := splitNative(id.Native)
	if err != nil {
		return mmail.Thread{}, err
	}

	anchor, err := p.Get(ctx, p.scoped(folderID, threadID))
	if err != nil {
		return mmail.Thread{}, err
	}

	thread := mmail.Thread{
		ID:       id,
		Account:  p.account.Alias,
		Subject:  anchor.Subject,
		Messages: []mmail.Message{anchor},
		Derived:  true,
	}

	query := url.Values{}
	query.Set("threadId", threadID)
	query.Set("limit", "200")
	query.Set("includeto", "true")

	var raw []message
	if err := p.get(ctx, "/accounts/"+p.accountID+"/messages/view", query, &raw); err != nil {
		// The anchor is already in hand and is a truthful, if narrow, answer. Failing the
		// whole call because the grouping could not be widened would lose it.
		return thread, nil
	}
	for _, m := range raw {
		converted := p.convert(m)
		if converted.ID.String() == anchor.ID.String() {
			continue
		}
		thread.Messages = append(thread.Messages, converted)
	}
	sort.SliceStable(thread.Messages, func(i, j int) bool {
		return thread.Messages[i].Date.Before(thread.Messages[j].Date)
	})
	if len(thread.Messages) > 0 {
		thread.Subject = thread.Messages[0].Subject
	}
	return thread, nil
}

func (p *Provider) GetAttachment(ctx context.Context, msg mmail.ScopedID, attachmentID string) (mmail.Attachment, error) {
	folderID, messageID, err := splitNative(msg.Native)
	if err != nil {
		return mmail.Attachment{}, err
	}

	// Not p.get. This endpoint answers with the file itself, not with the envelope every
	// other Zoho route uses, and asking for JSON is refused outright:
	//
	//	406 Not Acceptable  NOT_ACCEPTABLE  "The requested media type is not supported"
	//
	// The previous shape decoded an envelope with a base64 `content` field, which is what
	// the documentation describes and not what the mailbox returns. It had never run: Zoho
	// produced no attachment manifest, so no caller could obtain an id to pass here.
	path := fmt.Sprintf("/accounts/%s/folders/%s/messages/%s/attachments/%s",
		p.accountID, folderID, messageID, attachmentID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return mmail.Attachment{}, err
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return mmail.Attachment{}, p.wrap("GET "+path, 0, err)
	}
	defer resp.Body.Close()

	content, err := io.ReadAll(io.LimitReader(resp.Body, mmail.MaxAttachmentBytes))
	if err != nil {
		return mmail.Attachment{}, p.wrap("GET "+path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 {
		return mmail.Attachment{}, p.wrap("GET "+path, resp.StatusCode,
			fmt.Errorf("%s: %s", resp.Status, snippet(content)))
	}

	ref := mmail.AttachmentRef{ID: attachmentID, Size: int64(len(content))}

	// The manifest carries no content type, so this response is the only place one is
	// reported. A filename is in the manifest already, but the one here is authoritative
	// for the bytes actually returned.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if parsed, _, err := mime.ParseMediaType(ct); err == nil {
			ref.MimeType = parsed
		}
	}
	// Percent-decoded, because Zoho encodes the filename inside a plain `filename=` parameter
	// rather than using the `filename*` form that says an encoding is in use. The live
	// mailbox returned
	//
	//	reporter.example%21mail.example%211700000000%211700086399.zip
	//
	// for a file the manifest names reporter.example!mail.example!1700000000!1700086399.zip, so
	// taking the header at face value produces a worse name than not reading it at all.
	//
	// A name that does not decode is kept as it arrived: a filename containing a literal
	// percent is a stranger thing than one that is encoded, but mangling it would be worse
	// than leaving it alone.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil && params["filename"] != "" {
			ref.Filename = params["filename"]
			if decoded, err := url.PathUnescape(ref.Filename); err == nil {
				ref.Filename = decoded
			}
		}
	}

	return mmail.Attachment{AttachmentRef: ref, Content: content}, nil
}

func (p *Provider) convert(m message) mmail.Message {
	out := mmail.Message{
		ID:       p.scoped(m.FolderID.String(), m.MessageID.String()),
		Account:  p.account.Alias,
		ThreadID: p.scoped(m.FolderID.String(), threadOrMessage(m)),
		Subject:  m.Subject,
		Snippet:  strings.TrimSpace(m.Summary),
		Date:     firstTime(m.ReceivedTime, m.SentDate),
	}

	out.From = parseOne(firstNonEmpty(m.FromAddress, m.Sender))
	out.To = parseList(m.ToAddress)
	out.Cc = parseList(m.CCAddress)

	// Zoho reports read state as an integer where 0 means unread.
	if n, ok := asInt(m.Status); ok {
		out.Flags.Read = n != 0
	}
	out.Flags.Starred = isFollowUp(m.FlagID)
	if n, ok := asInt(m.HasAttach); ok && n > 0 {
		// The listing says an attachment exists but not which; a caller wanting the manifest
		// fetches the message. Recording a placeholder would invent an id that resolves to
		// nothing.
		out.Attachments = nil
	}
	return out
}

// threadOrMessage picks the id a thread is reached by.
//
// Zoho reports a threadId only on a listing already in threaded mode — threadedMails=true, or
// a threadId filter — so a message reached by search or by the ordinary listing arrives
// without one and its own id stands in. That guess is right for a message that started a
// thread and wrong for every reply, which is why the provider declares derived threading
// rather than presenting the result as authoritative.
func threadOrMessage(m message) string {
	if m.ThreadID != "" {
		return m.ThreadID.String()
	}
	return m.MessageID.String()
}

func firstTime(values ...json.RawMessage) time.Time {
	for _, v := range values {
		if parsed := zohoTime(v); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

// isFollowUp reads the flag Zoho answers with, in either of the two shapes it uses. The
// listing endpoint's own sample answers "flag_not_set" and the search endpoint's answers 2,
// so a reader that understood only one of them would report half the mailbox's stars.
func isFollowUp(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if n, ok := asInt(raw); ok {
		return n == flagFollowUp
	}
	return strings.EqualFold(strings.Trim(string(raw), `"`), flagNameFollowUp)
}

func asInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	s := strings.Trim(string(raw), `"`)
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func parseOne(v string) mmail.Address {
	list := parseList(v)
	if len(list) == 0 {
		return mmail.Address{}
	}
	return list[0]
}

func parseList(v string) []mmail.Address {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parsed, err := mail.ParseAddressList(v)
	if err != nil {
		// Malformed address lists are common in real mail. Keep the raw value rather than
		// dropping the field, so a human can still see who it claims to be from.
		return []mmail.Address{{Email: v}}
	}
	out := make([]mmail.Address, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, mmail.Address{Name: a.Name, Email: a.Address})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
