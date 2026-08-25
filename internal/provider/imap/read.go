package imap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Search finds messages in one mailbox.
//
// IMAP has no cursor: SEARCH returns every matching UID at once. Paging is therefore done
// here, by sorting the UIDs and fetching one window of them — the cursor carries the offset.
// That the aggregator cannot tell this apart from Gmail's page tokens is the seam working.
func (p *Provider) Search(ctx context.Context, q mmail.Query, cursor string) (mmail.Page[mmail.Message], error) {
	if q.HasAttach {
		// IMAP has no way to ask this. RFC 3501's SEARCH keys cover flags, dates, sizes,
		// headers and text, and there is nothing among them for whether a message carries an
		// attachment — that is a property of the MIME structure, which SEARCH does not see.
		//
		// The filter used to be dropped here in silence, which is the worst available answer:
		// the whole mailbox came back and every message in it was presented as one that has
		// an attachment.
		return mmail.Page[mmail.Message]{}, p.unsupported(mmail.CapRead,
			"an attachment filter",
			"IMAP's SEARCH has no key for whether a message carries an attachment, so this "+
				"cannot be asked of the server; search without it and read hasAttachments off "+
				"the results, or use a mailbox on a provider whose search understands it")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	mailbox := defaultMailbox
	if q.Label != "" {
		mailbox = string(q.Label)
	}
	if err := p.selectMailbox(mailbox, true); err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	// UID SEARCH, not SEARCH. The two differ in what the server answers with — sequence
	// numbers for one, UIDs for the other — and everything downstream here is UIDs: the ids
	// handed back to callers, the FETCH that follows, the whole addressing scheme.
	//
	// It was SEARCH, and the results were read as UIDs anyway. AllUIDs on a sequence-number
	// answer is empty, so every search against every IMAP mailbox returned no messages and
	// no error. The conformance suite did not catch it because its first assertion skips when
	// nothing matched — "point the harness at a mailbox with mail in it" — so a provider that
	// found nothing skipped every behavioural check that followed and reported a pass.
	data, err := p.client.UIDSearch(searchCriteria(q), &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		return mmail.Page[mmail.Message]{}, p.wrap("search", err)
	}

	uids := data.AllUIDs()
	// Newest first, matching every other provider. UIDs ascend with arrival, so descending
	// UID is a good proxy for descending date and avoids fetching everything to sort by it.
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })

	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 {
			return mmail.Page[mmail.Message]{}, p.wrap("search", errMalformedCursor(cursor))
		}
		offset = parsed
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	if offset >= len(uids) {
		return mmail.Page[mmail.Message]{}, nil
	}
	end := min(offset+limit, len(uids))
	window := uids[offset:end]

	messages, err := p.fetchEnvelopes(mailbox, window)
	if err != nil {
		return mmail.Page[mmail.Message]{}, err
	}

	page := mmail.Page[mmail.Message]{Items: messages}
	if end < len(uids) {
		page.Cursor = strconv.Itoa(end)
	}
	return page, nil
}

func (p *Provider) fetchEnvelopes(mailbox string, uids []imap.UID) ([]mmail.Message, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	set := imap.UIDSetNum(uids...)

	cmd := p.client.Fetch(set, &imap.FetchOptions{
		Envelope: true,
		Flags:    true,
		UID:      true,
	})
	defer cmd.Close()

	var out []mmail.Message
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, p.wrap("fetch", err)
		}
		out = append(out, p.convertEnvelope(mailbox, buf))
	}
	return out, p.wrap("fetch", cmd.Close())
}

func (p *Provider) convertEnvelope(mailbox string, buf *imapclient.FetchMessageBuffer) mmail.Message {
	out := mmail.Message{
		ID:      p.scoped(mailbox, buf.UID),
		Account: p.account.Alias,
		// IMAP has no thread id. The thread is derived on demand from headers, so the id
		// points back at this message and GetThread does the work.
		ThreadID: p.scoped(mailbox, buf.UID),
	}
	if env := buf.Envelope; env != nil {
		out.Subject = env.Subject
		out.Date = env.Date
		out.From = firstAddress(env.From)
		out.To = convertAddresses(env.To)
		out.Cc = convertAddresses(env.Cc)
	}
	for _, f := range buf.Flags {
		switch f {
		case imap.FlagSeen:
			out.Flags.Read = true
		case imap.FlagFlagged:
			out.Flags.Starred = true
		case imap.FlagDraft:
			out.Flags.Draft = true
		}
	}
	return out
}

func (p *Provider) Get(ctx context.Context, id mmail.ScopedID) (mmail.Message, error) {
	mailbox, uid, err := splitNative(id.Native)
	if err != nil {
		return mmail.Message{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.selectMailbox(mailbox, true); err != nil {
		return mmail.Message{}, err
	}

	set := imap.UIDSetNum(uid)
	cmd := p.client.Fetch(set, &imap.FetchOptions{
		Envelope:    true,
		Flags:       true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{}},
	})
	defer cmd.Close()

	msg := cmd.Next()
	if msg == nil {
		// An empty result for a specific uid means it is gone, which callers must be able to
		// distinguish from a transport failure.
		return mmail.Message{}, mmail.ErrNotFound
	}
	buf, err := msg.Collect()
	if err != nil {
		return mmail.Message{}, p.wrap("get", err)
	}

	out := p.convertEnvelope(mailbox, buf)
	for _, body := range buf.BodySection {
		if err := parseBody(body.Bytes, &out); err != nil {
			continue
		}
	}
	return out, nil
}

// parseBody walks the MIME tree for the text and HTML parts and the attachment manifest.
//
// An attachment is recorded under the IMAP section number of the part carrying it — "2", or
// "1.2" inside a nested multipart — because a section number is the only handle a later
// FETCH can name a part by, and the manifest is where a client gets it from. The numbering
// is the one the protocol defines: the children of a multipart count from one, a message
// that is not multipart at all is section 1 whole, and a part that is not itself a multipart
// is a leaf however much structure it may contain.
func parseBody(raw []byte, out *mmail.Message) error {
	entity, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return err
	}
	collectPart(entity, "", out)
	return nil
}

func collectPart(e *message.Entity, section string, out *mmail.Message) {
	if parts := e.MultipartReader(); parts != nil {
		for i := 1; ; i++ {
			part, err := parts.NextPart()
			if err != nil {
				return
			}
			collectPart(part, childSection(section, i), out)
		}
	}
	if section == "" {
		section = "1"
	}

	body, err := io.ReadAll(e.Body)
	if err != nil {
		return
	}
	contentType, _, _ := e.Header.ContentType()
	filename, inline, isAttachment := describePart(&e.Header)

	if !isAttachment {
		switch {
		case strings.HasPrefix(contentType, "text/plain") && out.Body.Text == "":
			out.Body.Text = string(body)
		case strings.HasPrefix(contentType, "text/html") && out.Body.HTML == "":
			out.Body.HTML = string(body)
		}
		return
	}
	out.Attachments = append(out.Attachments, mmail.AttachmentRef{
		ID:       section,
		Filename: filename,
		MimeType: contentType,
		Size:     int64(len(body)),
		Inline:   inline,
	})
}

// describePart reports whether a part is a file rather than message text, and what to call
// it. A named part counts even when it is marked inline, because an inline image is still
// something a client can ask for by name — which is how the Gmail provider reads one too.
func describePart(h *message.Header) (filename string, inline, attachment bool) {
	disposition, params, _ := h.ContentDisposition()
	filename = params["filename"]
	if filename == "" {
		_, typeParams, _ := h.ContentType()
		filename = typeParams["name"]
	}
	inline = strings.EqualFold(disposition, "inline")
	return filename, inline, strings.EqualFold(disposition, "attachment") || filename != ""
}

func childSection(parent string, index int) string {
	if parent == "" {
		return strconv.Itoa(index)
	}
	return parent + "." + strconv.Itoa(index)
}

func parseSection(id string) ([]int, error) {
	if id == "" {
		return nil, fmt.Errorf("empty attachment id")
	}
	fields := strings.Split(id, ".")
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("malformed attachment id %q: want an imap section number", id)
		}
		out = append(out, n)
	}
	return out, nil
}

// GetThread derives a conversation from headers, because IMAP has no thread id.
//
// Derived is true on the result, and the provider declares the derived_threads quirk, so an
// agent asked to "reply to the last message in this thread" can tell that the grouping was
// inferred rather than authoritative.
func (p *Provider) GetThread(ctx context.Context, id mmail.ScopedID) (mmail.Thread, error) {
	root, err := p.Get(ctx, id)
	if err != nil {
		return mmail.Thread{}, err
	}

	thread := mmail.Thread{
		ID:       id,
		Account:  p.account.Alias,
		Subject:  root.Subject,
		Messages: []mmail.Message{root},
		Derived:  true,
	}

	// Subject-based grouping is the fallback the protocol leaves available. It is imprecise,
	// which is exactly what Derived warns about.
	normalized := normalizeSubject(root.Subject)
	if normalized == "" {
		return thread, nil
	}

	related, err := p.Search(ctx, mmail.Query{Subject: normalized, Limit: 50}, "")
	if err != nil {
		return thread, nil
	}
	for _, m := range related.Items {
		if m.ID.String() == root.ID.String() {
			continue
		}
		if normalizeSubject(m.Subject) == normalized {
			thread.Messages = append(thread.Messages, m)
		}
	}
	sort.Slice(thread.Messages, func(i, j int) bool {
		return thread.Messages[i].Date.Before(thread.Messages[j].Date)
	})
	return thread, nil
}

// GetAttachment fetches the bytes of one part.
//
// The id is the section number the manifest recorded, so the part is fetched directly:
// asking for a 4 KB spreadsheet does not pull down the 20 MB video sitting next to it, which
// is what reading the message and picking the part out of the tree would cost.
//
// This used to return the manifest entry and nothing else — no Content at all — so
// mail_get_attachment answered every request on an IMAP mailbox with a zero-byte file and a
// success status, which a client cannot tell from an attachment that is genuinely empty.
func (p *Provider) GetAttachment(ctx context.Context, msgID mmail.ScopedID, attachmentID string) (mmail.Attachment, error) {
	mailbox, uid, err := splitNative(msgID.Native)
	if err != nil {
		return mmail.Attachment{}, err
	}
	section, err := parseSection(attachmentID)
	if err != nil {
		// An id this provider never issued names no part, which is the same answer as a part
		// that has since gone.
		return mmail.Attachment{}, mmail.ErrNotFound
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.selectMailbox(mailbox, true); err != nil {
		return mmail.Attachment{}, err
	}

	// The part's own MIME header travels with it: it carries the transfer encoding the bytes
	// have to be decoded from, and the name and type to answer with. PEEK because downloading
	// a file is not reading the message, and a plain fetch would set \Seen on it.
	header := &imap.FetchItemBodySection{Part: section, Specifier: imap.PartSpecifierMIME, Peek: true}
	content := &imap.FetchItemBodySection{Part: section, Peek: true}

	cmd := p.client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{header, content},
	})
	defer cmd.Close()

	msg := cmd.Next()
	if msg == nil {
		return mmail.Attachment{}, mmail.ErrNotFound
	}
	buf, err := msg.Collect()
	if err != nil {
		return mmail.Attachment{}, p.wrap("get_attachment", err)
	}
	if err := cmd.Close(); err != nil {
		return mmail.Attachment{}, p.wrap("get_attachment", err)
	}

	rawHeader, rawBody := bodySections(buf)
	if len(rawHeader) == 0 {
		// A section the message does not have comes back empty rather than as an error, and
		// answering with those empty bytes is the failure this whole method exists to end.
		return mmail.Attachment{}, mmail.ErrNotFound
	}

	parsed, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(rawHeader)))
	if err != nil {
		return mmail.Attachment{}, p.wrap("get_attachment", err)
	}
	partHeader := message.Header{Header: parsed}

	filename, inline, isAttachment := describePart(&partHeader)
	if !isAttachment {
		// The manifest only ever offers parts that are files. A section naming the message
		// text would otherwise hand back the body under an attachment's name.
		return mmail.Attachment{}, mmail.ErrNotFound
	}

	data, err := decodePart(partHeader.Get("Content-Transfer-Encoding"), rawBody)
	if err != nil {
		return mmail.Attachment{}, p.wrap("get_attachment", err)
	}
	contentType, _, _ := partHeader.ContentType()

	return mmail.Attachment{
		AttachmentRef: mmail.AttachmentRef{
			ID:       attachmentID,
			Filename: filename,
			MimeType: contentType,
			Size:     int64(len(data)),
			Inline:   inline,
		},
		Content: data,
	}, nil
}

// bodySections separates the two sections of one fetch: the part's MIME header, and the part
// itself.
func bodySections(buf *imapclient.FetchMessageBuffer) (header, body []byte) {
	for _, section := range buf.BodySection {
		if section.Section == nil {
			continue
		}
		if section.Section.Specifier == imap.PartSpecifierMIME {
			header = section.Bytes
			continue
		}
		body = section.Bytes
	}
	return header, body
}

// decodePart undoes the transfer encoding and nothing else.
//
// Content means the bytes the file had before it was put in a message, everywhere in the
// model — so a charset conversion does not belong here: it would rewrite a text attachment
// into something that no longer hashes to what was sent.
func decodePart(encoding string, raw []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return raw, nil
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(raw)))
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	default:
		return nil, fmt.Errorf("attachment uses an unsupported content transfer encoding %q", encoding)
	}
}

// searchCriteria renders the canonical query as RFC 3501 SEARCH keys.
//
// Every field it does not render has to be refused by Search before it gets here, not
// dropped: a SEARCH that omits a term the caller asked for succeeds and answers with mail
// that does not match it. HasAttach is the one this provider has no key for; the rest map
// onto keys the protocol defines.
//
// The date bounds are wider than they look and deliberately so. SINCE and BEFORE compare
// against INTERNALDATE with the time of day discarded, so a bound is a whole day rather than
// an instant — and BEFORE is exclusive of the day named, which is what the canonical Before
// means anyway.
func searchCriteria(q mmail.Query) *imap.SearchCriteria {
	c := &imap.SearchCriteria{}
	if q.From != "" {
		c.Header = append(c.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: q.From})
	}
	if q.To != "" {
		c.Header = append(c.Header, imap.SearchCriteriaHeaderField{Key: "To", Value: q.To})
	}
	if q.Subject != "" {
		c.Header = append(c.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: q.Subject})
	}
	if q.Raw != "" {
		c.Text = append(c.Text, q.Raw)
	}
	if q.Unread {
		c.NotFlag = append(c.NotFlag, imap.FlagSeen)
	}
	if q.Starred {
		c.Flag = append(c.Flag, imap.FlagFlagged)
	}
	if !q.After.IsZero() {
		c.Since = q.After
	}
	if !q.Before.IsZero() {
		c.Before = q.Before
	}
	return c
}

func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "re:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(lower, "fwd:"):
			s = strings.TrimSpace(s[4:])
		case strings.HasPrefix(lower, "fw:"):
			s = strings.TrimSpace(s[3:])
		default:
			return s
		}
	}
}

func firstAddress(in []imap.Address) mmail.Address {
	converted := convertAddresses(in)
	if len(converted) == 0 {
		return mmail.Address{}
	}
	return converted[0]
}

func convertAddresses(in []imap.Address) []mmail.Address {
	out := make([]mmail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, mmail.Address{Name: a.Name, Email: a.Addr()})
	}
	return out
}

func errMalformedCursor(c string) error {
	return &malformedCursor{cursor: c}
}

type malformedCursor struct{ cursor string }

func (m *malformedCursor) Error() string { return "malformed cursor " + m.cursor }

var _ = time.Time{}
