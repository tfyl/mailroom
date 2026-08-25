package microsoft

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// message is the slice of Graph's message resource mailroom reads. Everything here is named
// in the $select below, because Graph returns a much larger object otherwise and a listing
// then costs several times what it needs to.
type message struct {
	ID               string          `json:"id"`
	ConversationID   string          `json:"conversationId"`
	ParentFolderID   string          `json:"parentFolderId"`
	Subject          string          `json:"subject"`
	BodyPreview      string          `json:"bodyPreview"`
	ReceivedDateTime string          `json:"receivedDateTime"`
	SentDateTime     string          `json:"sentDateTime"`
	IsRead           bool            `json:"isRead"`
	IsDraft          bool            `json:"isDraft"`
	HasAttachments   bool            `json:"hasAttachments"`
	Categories       []string        `json:"categories"`
	From             *recipient      `json:"from"`
	Sender           *recipient      `json:"sender"`
	ToRecipients     []recipient     `json:"toRecipients"`
	CcRecipients     []recipient     `json:"ccRecipients"`
	BccRecipients    []recipient     `json:"bccRecipients"`
	Flag             *followupFlag   `json:"flag"`
	Body             *itemBody       `json:"body"`
	Attachments      []attachmentRef `json:"attachments"`
}

type recipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type itemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// followupFlag is Outlook's flag-for-follow-up, which is the nearest thing it has to a star.
// flagStatus is one of notFlagged, complete or flagged.
type followupFlag struct {
	FlagStatus string `json:"flagStatus"`
}

const flagged = "flagged"
const notFlagged = "notFlagged"

type attachmentRef struct {
	ODataType    string `json:"@odata.type"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	IsInline     bool   `json:"isInline"`
	ContentBytes []byte `json:"contentBytes"`
}

// messagePage is what every message listing endpoint answers with.
type messagePage struct {
	Value    []message `json:"value"`
	NextLink string    `json:"@odata.nextLink"`
}

// listFields is the $select for a listing: enough to render a result row and to act on it,
// and no body. A search over fifty messages that dragged fifty HTML bodies with it would be
// an order of magnitude more traffic for something no caller of Search reads.
const listFields = "id,conversationId,parentFolderId,subject,bodyPreview,receivedDateTime," +
	"sentDateTime,isRead,isDraft,hasAttachments,categories,from,sender,toRecipients," +
	"ccRecipients,bccRecipients,flag"

const messageFields = listFields + ",body"

const defaultPageSize = 50

// Search finds messages, translating the canonical query into whichever of Graph's two query
// mechanisms can express it: $filter evaluates OData against indexed properties and can be
// ordered, $search runs the mailbox's full-text index. The choice is made once, from the
// query — anything with a free-text term goes to $search, everything else to $filter.
//
// The two are never combined, and the reason is not that Microsoft forbids it. The
// documentation used to say $search on messages could carry neither $filter nor $orderby and
// no longer says anything either way, which would be an argument for trying it — except that
// Graph's own known-issues page says an unsupported combination of query parameters "might
// fail silently". A filter Graph ignores comes back as a full unfiltered page that looks
// exactly like an answer, and a mailbox gateway is the wrong place to find that out. So the
// half of a query that cannot ride along is refused by name instead.
func (p *Provider) Search(ctx context.Context, q mmail.Query, cursor string) (mmail.Page[mmail.Message], error) {
	if cursor != "" {
		var page messagePage
		if err := p.follow(ctx, cursor, &page); err != nil {
			return mmail.Page[mmail.Message]{}, err
		}
		// A nextLink carries the original query parameters, so a filter Graph ignored on the
		// first page it ignores on this one too. Checking only the first page would make the
		// answer correct exactly until somebody paged.
		page.Value = verified(q, page.Value)
		return p.page(page), nil
	}

	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = defaultPageSize
	}

	// A folder is addressed by path rather than by filter: parentFolderId is filterable, but
	// scoping the collection is what lets a search inside a folder page independently of the
	// rest of the mailbox.
	path := "/me/messages"
	if q.Label != "" {
		kind, native, err := splitLabelID(q.Label)
		if err != nil {
			return mmail.Page[mmail.Message]{}, err
		}
		if kind == labelFolder {
			path = "/me/mailFolders/" + escapeID(native) + "/messages"
		}
	}

	query := url.Values{}
	query.Set("$select", listFields)
	query.Set("$top", strconv.Itoa(limit))

	if term := searchTerm(q); term != "" {
		if q.Unread || q.Starred {
			// Both are ordinary $filter predicates and neither has a KQL equivalent Exchange
			// will honour reliably, so serving them alongside a search would mean answering a
			// narrower question than the one asked and saying nothing about it.
			return mmail.Page[mmail.Message]{}, p.unsupported(mmail.CapRead,
				"an unread or starred filter alongside search terms",
				"Graph evaluates $search on messages without $filter, so these cannot be "+
					"combined; search on its own, or drop the search terms and filter")
		}
		if q.Label != "" && !strings.HasPrefix(path, "/me/mailFolders/") {
			return mmail.Page[mmail.Message]{}, p.unsupported(mmail.CapRead,
				"a category filter alongside search terms",
				"a category is matched with $filter, which Graph will not evaluate beside "+
					"$search; search within a folder instead, or filter without search terms")
		}
		// A $search answers at most a thousand results in total, however far a caller pages,
		// and is ordered by sent date rather than by anything asked for here.
		query.Set("$search", `"`+term+`"`)
	} else {
		filter, orderable := filterExpression(q)
		if filter != "" {
			query.Set("$filter", filter)
		}
		// Newest first, but only where Exchange will serve it. Its documented rule is that
		// every property in $orderby must also appear in $filter, and appear there before any
		// property that is not in $orderby — a request that breaks it is refused outright with
		// InefficientFilter, "the restriction or sort order is too complex for this operation".
		// So the date clauses are written first and the sort is asked for only when that makes
		// it legal. A page that comes back in Exchange's own order is a smaller loss than a
		// search that fails.
		if orderable {
			query.Set("$orderby", "receivedDateTime desc")
		}
	}

	var page messagePage
	if err := p.get(ctx, path, query, &page); err != nil {
		return mmail.Page[mmail.Message]{}, err
	}
	page.Value = verified(q, page.Value)
	return p.page(page), nil
}

// verified drops results that do not match the predicates the filter asked for.
//
// This is the same defence GetThread applies to conversationId, for the same reason and with
// more at stake. Microsoft publishes no filterable-property list for the message resource;
// the only $filter examples anywhere in the documentation are on from, receivedDateTime,
// isRead, subject and importance. Filtering on flag/flagStatus and on categories appears
// nowhere at all, and Graph's known-issues page says a query parameter it does not support
// "might fail silently" — so an ignored filter comes back as a full unfiltered page that
// looks exactly like an answer, and the caller reads unstarred mail as starred.
//
// The attachment predicate is here for a narrower reason: there is no $filter example for
// hasAttachments either, and on the search side Microsoft's property table calls it
// hasAttachment while the worked example beside it says hasAttachments. Both cannot be right.
//
// Checking costs one comparison per message and makes the worst case a short page rather
// than a wrong one. What is not checked is the rest — isRead, the date bounds, the folder
// scope — because Microsoft publishes worked examples for those, and re-testing a filter the
// service is known to honour would only invent a second place for the semantics to disagree.
func verified(q mmail.Query, in []message) []message {
	category := ""
	if q.Label != "" {
		if kind, native, err := splitLabelID(q.Label); err == nil && kind == labelCategory {
			category = native
		}
	}
	if !q.Starred && !q.HasAttach && category == "" {
		return in
	}

	out := in[:0]
	for _, m := range in {
		switch {
		case q.Starred && (m.Flag == nil || m.Flag.FlagStatus != flagged):
		case q.HasAttach && !m.HasAttachments:
		case category != "" && !containsFold(m.Categories, category):
		default:
			out = append(out, m)
		}
	}
	return out
}

func (p *Provider) page(raw messagePage) mmail.Page[mmail.Message] {
	items := make([]mmail.Message, 0, len(raw.Value))
	for _, m := range raw.Value {
		items = append(items, p.convert(m))
	}
	// The nextLink is carried whole. Graph documents it as opaque, and picking the skip token
	// out of it would be reconstructing a query the service has already written down.
	return mmail.Page[mmail.Message]{Items: items, Cursor: raw.NextLink}
}

// searchTerm renders the free-text half of a query as the KQL Exchange's search index takes.
// Empty when the query has no free-text terms, which is what selects the $filter path.
//
// hasAttachments is spelled as Microsoft's own working example spells it. Their property table
// for the same syntax says hasAttachment, singular, and the two cannot both be right; the
// example is the half that has plainly been run.
func searchTerm(q mmail.Query) string {
	var parts []string
	if q.Raw != "" {
		parts = append(parts, q.Raw)
	}
	if q.From != "" {
		parts = append(parts, "from:"+quoteKQL(q.From))
	}
	if q.To != "" {
		parts = append(parts, "to:"+quoteKQL(q.To))
	}
	if q.Subject != "" {
		parts = append(parts, "subject:"+quoteKQL(q.Subject))
	}
	if len(parts) == 0 {
		return ""
	}
	if q.HasAttach {
		parts = append(parts, "hasAttachments:true")
	}
	if !q.After.IsZero() {
		parts = append(parts, "received>="+q.After.UTC().Format("2006-01-02"))
	}
	if !q.Before.IsZero() {
		parts = append(parts, "received<="+q.Before.UTC().Format("2006-01-02"))
	}
	return strings.Join(parts, " AND ")
}

// quoteKQL wraps a value so that spaces in it stay one term, and strips the quote character
// rather than escaping it: KQL has no escape for a quote inside a quoted phrase, so a value
// carrying one cannot be expressed and dropping it is the closest honest rendering.
func quoteKQL(v string) string {
	v = strings.ReplaceAll(v, `"`, "")
	if !strings.ContainsAny(v, " \t") {
		return v
	}
	return `"` + v + `"`
}

// filterExpression renders the structured half of a query as OData, and reports whether the
// result may be ordered by receivedDateTime.
//
// The date clauses come first because Exchange's ordering rule is positional: a property being
// sorted on has to appear in the filter before any property that is not being sorted on.
// Ordering is legal when the filter is empty, or when it opens with the date — which is what
// writing those clauses first arranges.
func filterExpression(q mmail.Query) (expression string, orderable bool) {
	var parts []string
	if !q.After.IsZero() {
		parts = append(parts, "receivedDateTime ge "+odataTime(q.After))
	}
	if !q.Before.IsZero() {
		parts = append(parts, "receivedDateTime lt "+odataTime(q.Before))
	}
	dated := len(parts) > 0

	if q.Unread {
		parts = append(parts, "isRead eq false")
	}
	if q.Starred {
		parts = append(parts, "flag/flagStatus eq 'flagged'")
	}
	if q.HasAttach {
		parts = append(parts, "hasAttachments eq true")
	}
	if q.Label != "" {
		if kind, native, err := splitLabelID(q.Label); err == nil && kind == labelCategory {
			parts = append(parts, "categories/any(c:c eq "+quoteOData(native)+")")
		}
	}
	return strings.Join(parts, " and "), dated || len(parts) == 0
}

func odataTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// quoteOData renders a string literal. OData escapes a single quote by doubling it, and
// leaving that undone is how a mailbox with an apostrophe in a category name produces a
// request that fails to parse.
func quoteOData(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func (p *Provider) Get(ctx context.Context, id mmail.ScopedID) (mmail.Message, error) {
	query := url.Values{}
	query.Set("$select", messageFields)

	var raw message
	if err := p.get(ctx, "/me/messages/"+escapeID(id.Native), query, &raw); err != nil {
		return mmail.Message{}, err
	}

	out := p.convert(raw)
	if raw.HasAttachments {
		// The manifest is a second call: Graph carries attachments on a message only when
		// they are expanded, and expanding them drags every attachment's bytes along with the
		// message. Listing them separately is what keeps reading a message with a large file
		// on it from downloading that file.
		refs, err := p.listAttachments(ctx, id.Native)
		if err != nil {
			return mmail.Message{}, err
		}
		out.Attachments = refs
	}
	return out, nil
}

// GetThread lists the messages sharing a conversation id.
//
// Exchange assigns the conversation, so the grouping is authoritative and Derived is
// correspondingly false. The sort is done here rather than with $orderby: receivedDateTime is
// not the property being filtered on, and Exchange refuses that combination outright.
//
// The filter is also checked rather than trusted, which is unusual enough to say why.
// Microsoft documents no filterable-property list for messages at all, and there is not one
// official example anywhere of filtering on conversationId — it is common practice with
// nothing behind it. Graph's known-issues page says an unsupported filter "might fail
// silently", and a silently ignored filter here would return the whole mailbox and call it a
// conversation. Dropping what does not belong costs one comparison and makes the worst case a
// slow answer instead of a wrong one.
func (p *Provider) GetThread(ctx context.Context, id mmail.ScopedID) (mmail.Thread, error) {
	query := url.Values{}
	query.Set("$select", listFields)
	query.Set("$top", "200")
	query.Set("$filter", "conversationId eq "+quoteOData(id.Native))

	var raw messagePage
	if err := p.get(ctx, "/me/messages", query, &raw); err != nil {
		return mmail.Thread{}, err
	}

	thread := mmail.Thread{ID: id, Account: p.account.Alias, Derived: false}
	for _, m := range raw.Value {
		if m.ConversationID != id.Native {
			continue
		}
		thread.Messages = append(thread.Messages, p.convert(m))
	}
	sort.SliceStable(thread.Messages, func(i, j int) bool {
		return thread.Messages[i].Date.Before(thread.Messages[j].Date)
	})
	if len(thread.Messages) > 0 {
		thread.Subject = thread.Messages[0].Subject
	}
	return thread, nil
}

// listAttachments reads the manifest without the payloads.
//
// The $select is what asks Graph to leave contentBytes out, and the shape decoded into has no
// field for them either — because Graph's known-issues page records that unsupported query
// parameters can be dropped in silence, and a $select silently ignored on a message with a
// hundred-megabyte attachment would otherwise pull the whole thing into memory to read a
// filename off it.
func (p *Provider) listAttachments(ctx context.Context, messageID string) ([]mmail.AttachmentRef, error) {
	query := url.Values{}
	query.Set("$select", "id,name,contentType,size,isInline")

	var page struct {
		Value []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
			Size        int64  `json:"size"`
			IsInline    bool   `json:"isInline"`
		} `json:"value"`
	}
	if err := p.get(ctx, "/me/messages/"+escapeID(messageID)+"/attachments", query, &page); err != nil {
		return nil, err
	}
	out := make([]mmail.AttachmentRef, 0, len(page.Value))
	for _, a := range page.Value {
		out = append(out, mmail.AttachmentRef{
			ID: a.ID, Filename: a.Name, MimeType: a.ContentType,
			Size: a.Size, Inline: a.IsInline,
		})
	}
	return out, nil
}

// GetAttachment fetches one attachment's bytes.
//
// Only a fileAttachment has bytes of its own. An item attachment is a whole message or event
// embedded in this one and a reference attachment is a link to cloud storage, and neither has
// a contentBytes to hand back — so both are refused by name rather than returned as an empty
// file, which is what a caller would otherwise write to disk and wonder about.
func (p *Provider) GetAttachment(ctx context.Context, msg mmail.ScopedID, attachmentID string) (mmail.Attachment, error) {
	var raw attachmentRef
	path := "/me/messages/" + escapeID(msg.Native) + "/attachments/" + escapeID(attachmentID)
	if err := p.get(ctx, path, nil, &raw); err != nil {
		return mmail.Attachment{}, err
	}
	// Matched as a substring because Microsoft's own examples disagree about the leading hash:
	// the collection shows microsoft.graph.fileAttachment and the single-item read shows
	// #microsoft.graph.fileAttachment, on adjacent pages.
	if !strings.Contains(raw.ODataType, "fileAttachment") {
		return mmail.Attachment{}, p.unsupported(mmail.CapAttachments,
			"fetching a "+odataShortName(raw.ODataType),
			"only a file attachment carries bytes; an item attachment is an embedded message "+
				"or event and a reference attachment is a link to cloud storage")
	}

	return mmail.Attachment{
		AttachmentRef: mmail.AttachmentRef{
			ID: raw.ID, Filename: raw.Name, MimeType: raw.ContentType,
			Size: raw.Size, Inline: raw.IsInline,
		},
		// contentBytes arrives base64-encoded, which encoding/json decodes into a []byte
		// field without any help.
		Content: raw.ContentBytes,
	}, nil
}

func odataShortName(t string) string {
	if _, name, ok := strings.Cut(t, "graph."); ok {
		return name
	}
	if t == "" {
		return "attachment of an unnamed kind"
	}
	return t
}

func (p *Provider) convert(m message) mmail.Message {
	out := mmail.Message{
		ID:       p.scoped(m.ID),
		Account:  p.account.Alias,
		ThreadID: p.scoped(threadOrMessage(m)),
		Subject:  m.Subject,
		Snippet:  strings.TrimSpace(m.BodyPreview),
		Date:     firstTime(m.ReceivedDateTime, m.SentDateTime),
		Flags: mmail.Flags{
			Read:    m.IsRead,
			Starred: m.Flag != nil && m.Flag.FlagStatus == flagged,
			Draft:   m.IsDraft,
		},
	}

	out.From = convertAddress(firstRecipient(m.From, m.Sender))
	out.To = convertAddresses(m.ToRecipients)
	out.Cc = convertAddresses(m.CcRecipients)
	out.Bcc = convertAddresses(m.BccRecipients)
	out.Labels = labelsOf(m)

	if m.Body != nil {
		if strings.EqualFold(m.Body.ContentType, "html") {
			out.Body.HTML = m.Body.Content
		} else {
			out.Body.Text = m.Body.Content
		}
	}
	return out
}

// labelsOf reports where a message sits and what it is tagged with, in the one namespaced
// form the label model uses. The folder comes first because it is the exclusive one: a caller
// reading the list in order sees the placement before the tags added alongside it.
func labelsOf(m message) []mmail.LabelID {
	var out []mmail.LabelID
	if m.ParentFolderID != "" {
		out = append(out, folderLabel(m.ParentFolderID))
	}
	for _, c := range m.Categories {
		out = append(out, categoryLabel(c))
	}
	return out
}

func threadOrMessage(m message) string {
	if m.ConversationID != "" {
		return m.ConversationID
	}
	return m.ID
}

func firstRecipient(values ...*recipient) *recipient {
	for _, v := range values {
		if v != nil && v.EmailAddress.Address != "" {
			return v
		}
	}
	return nil
}

func convertAddress(r *recipient) mmail.Address {
	if r == nil {
		return mmail.Address{}
	}
	return mmail.Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address}
}

func convertAddresses(in []recipient) []mmail.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]mmail.Address, 0, len(in))
	for i := range in {
		out = append(out, convertAddress(&in[i]))
	}
	return out
}

func firstTime(values ...string) time.Time {
	for _, v := range values {
		if v == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// asRecipients renders addresses in the shape every write path takes them.
//
// Empty is an empty collection, never nil. A nil slice marshals to JSON null, and Graph
// refuses a null collection on POST /me/messages with UnableToDeserializePostBody — which
// took out drafting and sending entirely, because an ordinary message has no cc and no bcc.
// An empty array is also the right thing on the PATCH path, where it clears the recipients a
// draft already had; omitting the property would leave them in place.
func asRecipients(in []mmail.Address) []recipient {
	if len(in) == 0 {
		return []recipient{}
	}
	out := make([]recipient, 0, len(in))
	for _, a := range in {
		var r recipient
		r.EmailAddress.Name, r.EmailAddress.Address = a.Name, a.Email
		out = append(out, r)
	}
	return out
}
