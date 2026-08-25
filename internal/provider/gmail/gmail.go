// Package gmail implements the mail provider interfaces against the Gmail API.
package gmail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/tfyl/mailroom/internal/mail"
)

// Provider talks to one Gmail mailbox. One instance per linked account.
type Provider struct {
	svc     *gmail.Service
	account mail.Account
}

// New builds a provider for an account from a token source.
//
// The source rather than the stored refresh token it was built from: a refresh may return a
// *new* refresh token and invalidate the old one, and the only place that can be acted on is
// where the credential is stored. Handing this a source keeps that decision with whoever owns
// the store — see app.Providers, which builds one that writes a rotation back.
func New(ctx context.Context, account mail.Account, source oauth2.TokenSource) (*Provider, error) {
	svc, err := gmail.NewService(ctx, option.WithTokenSource(source))
	if err != nil {
		return nil, err
	}
	return &Provider{svc: svc, account: account}, nil
}

func (p *Provider) ID() mail.ProviderID { return mail.ProviderGmail }

// Capabilities is derived from the interfaces this type implements, so it cannot drift out
// of step with what the provider actually does.
func (p *Provider) Capabilities() mail.Set { return mail.DerivedCapabilities(p) }

// Quirks is empty: Gmail threads are native, labels are non-exclusive, batch is supported,
// and its search syntax is the one the canonical Query maps onto most directly.
func (p *Provider) Quirks() []mail.Quirk { return nil }

// --- MessageReader ---

// Search lists matching message ids and then fetches metadata for each.
//
// Gmail's list endpoint returns bare ids, so headers cost one request per message. They run
// concurrently with a small bound: enough to keep a page fast, low enough not to trip
// per-user rate limits and turn one search into a throttle for everything else.
func (p *Provider) Search(ctx context.Context, q mail.Query, cursor string) (mail.Page[mail.Message], error) {
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	call := p.svc.Users.Messages.List("me").
		Q(buildQuery(q)).
		MaxResults(int64(limit)).
		Context(ctx)
	if q.Label != "" {
		// The label goes in its own parameter rather than into the query string, because the
		// two take different things. labelIds takes the ids users.labels.list hands back —
		// "Only return messages with labels that match all of the specified label IDs" — while
		// the `label:` search operator is the one from the Gmail search box, whose documented
		// examples are display names. A user label's id is not its name, and Gmail does not
		// refuse a label token it does not recognise: it matches nothing. So a label-scoped
		// search came back empty and successful, which is the one answer a caller cannot tell
		// from an empty mailbox.
		call = call.LabelIds(string(q.Label))
	}
	if cursor != "" {
		call = call.PageToken(cursor)
	}

	resp, err := call.Do()
	if err != nil {
		return mail.Page[mail.Message]{}, p.wrap("search", err)
	}

	messages := make([]mail.Message, len(resp.Messages))
	errs := make([]error, len(resp.Messages))

	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, ref := range resp.Messages {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			m, err := p.svc.Users.Messages.Get("me", id).Format("metadata").
				MetadataHeaders("From", "To", "Cc", "Subject", "Date").Context(ctx).Do()
			if err != nil {
				errs[i] = err
				return
			}
			messages[i] = p.convert(m, false)
		}(i, ref.Id)
	}
	wg.Wait()

	// One unreadable message should not lose the page. Drop it and keep going; the caller
	// sees a shorter page rather than an error for the whole search.
	out := make([]mail.Message, 0, len(messages))
	for i, m := range messages {
		if errs[i] != nil || m.ID.Zero() {
			continue
		}
		out = append(out, m)
	}

	return mail.Page[mail.Message]{Items: out, Cursor: resp.NextPageToken}, nil
}

func (p *Provider) Get(ctx context.Context, id mail.ScopedID) (mail.Message, error) {
	m, err := p.svc.Users.Messages.Get("me", id.Native).Format("full").Context(ctx).Do()
	if err != nil {
		return mail.Message{}, p.wrap("get_message", err)
	}
	return p.convert(m, true), nil
}

// --- ThreadReader ---

func (p *Provider) GetThread(ctx context.Context, id mail.ScopedID) (mail.Thread, error) {
	t, err := p.svc.Users.Threads.Get("me", id.Native).Format("full").Context(ctx).Do()
	if err != nil {
		return mail.Thread{}, p.wrap("get_thread", err)
	}

	thread := mail.Thread{
		ID:      mail.ScopedID{Account: p.account.ID, Native: t.Id},
		Account: p.account.Alias,
		// Gmail groups conversations itself, so this is authoritative rather than inferred.
		Derived: false,
	}
	for _, m := range t.Messages {
		msg := p.convert(m, true)
		if thread.Subject == "" {
			thread.Subject = msg.Subject
		}
		thread.Messages = append(thread.Messages, msg)
	}
	return thread, nil
}

// --- AttachmentReader ---

func (p *Provider) GetAttachment(ctx context.Context, msg mail.ScopedID, attachmentID string) (mail.Attachment, error) {
	body, err := p.svc.Users.Messages.Attachments.Get("me", msg.Native, attachmentID).Context(ctx).Do()
	if err != nil {
		return mail.Attachment{}, p.wrap("get_attachment", err)
	}
	data, err := decodeBase64URL(body.Data)
	if err != nil {
		return mail.Attachment{}, fmt.Errorf("decoding attachment: %w", err)
	}

	ref := mail.AttachmentRef{ID: attachmentID, Size: body.Size}

	// Gmail's attachments.get answers with the bytes and a size and nothing else — no
	// filename, no content type. Both live on the message part that refers to the
	// attachment, so they are read back from the message.
	//
	// This matters beyond tidiness: the filename and content type go straight onto the
	// download link mailroom hands out, so without them every Gmail attachment arrived
	// unnamed and untyped. Microsoft and IMAP both return them from the fetch itself, which
	// is why it went unnoticed — the field was populated everywhere it was looked at.
	//
	// Matched on size, not on the id, because a Gmail attachment id is not stable: two
	// messages.get calls against the same message answer with different ids for the same
	// parts. Measured on a live mailbox — the same attachment came back as
	// "ANGjdJ_Nwd5qs…" and then "ANGjdJ-va_CK-…", same filename, same bytes. Ids already
	// handed out keep working, so this is a comparison problem rather than an expiry one,
	// but it means the obvious lookup silently matches nothing.
	//
	// Size is the discriminator that survives. Where two parts are byte-identical in length
	// there is no way to tell them apart, so the reference is left sparse rather than
	// labelled with a filename that might belong to the other one: an attachment downloaded
	// under the wrong name is worse than one downloaded under none.
	//
	// A failure to read the message back is likewise not a failure to fetch the attachment.
	// The bytes are already in hand and those are what the caller asked for.
	if m, err := p.svc.Users.Messages.Get("me", msg.Native).Format("full").Context(ctx).Do(); err == nil {
		var carrier mail.Message
		collectAttachmentRefs(m.Payload, &carrier)

		if match := uniqueRefBySize(carrier.Attachments, ref.Size); match != nil {
			ref.Filename, ref.MimeType, ref.Inline = match.Filename, match.MimeType, match.Inline
		}
	}

	return mail.Attachment{AttachmentRef: ref, Content: data}, nil
}

// uniqueRefBySize finds the one part of a given length, or reports that there is not one.
//
// Nil for "no part that size" and nil for "more than one" are deliberately the same answer:
// both mean the filename cannot be established, and the caller does the same thing either
// way. Guessing between two same-sized attachments would label a download with the other
// one's name, which is worse than leaving it unnamed.
func uniqueRefBySize(refs []mail.AttachmentRef, size int64) *mail.AttachmentRef {
	var found *mail.AttachmentRef
	for i := range refs {
		if refs[i].Size != size {
			continue
		}
		if found != nil {
			return nil
		}
		found = &refs[i]
	}
	return found
}

// wrap turns a Gmail API error into one of mailroom's typed failures.
//
// The distinction that matters most is an expired credential: retrying will never fix it,
// and the operator has to re-link the mailbox. Reporting that as a generic provider error
// would leave a client retrying forever against a mailbox that needs a human.
func (p *Provider) wrap(op string, err error) error {
	if err == nil {
		return nil
	}

	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		// Not every refusal from the token endpoint means the credential is dead, and the
		// consequence of getting this wrong is not symmetric. ErrNeedsReauth is persisted —
		// Gate.Observe writes needs_reauth on the account, and every later call is then
		// refused before it reaches Google — so a rate limit or a bad ten minutes at
		// accounts.google.com locks a perfectly good mailbox until a human re-links it.
		//
		// Only the transient shapes are carved out, and narrowly. Anything unrecognised
		// still reports as expired, because a mailbox that genuinely needs a human is worse
		// left looking healthy: a client would retry it forever and nobody would be told.
		if status, retryable := transientTokenFailure(retrieveErr); retryable {
			return &mail.ProviderError{
				Provider: mail.ProviderGmail, Account: p.account.Alias,
				Address: p.account.Address, Op: op,
				Retryable: true, RetryIn: status, Err: err,
			}
		}
		return mail.ErrNeedsReauth
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusUnauthorized:
			return mail.ErrNeedsReauth
		case http.StatusForbidden:
			// Gmail reports both quota exhaustion and genuine permission failures as 403;
			// the reason string is what separates them.
			if strings.Contains(apiErr.Message, "rateLimitExceeded") ||
				strings.Contains(apiErr.Message, "userRateLimitExceeded") {
				return &mail.ProviderError{
					Provider: mail.ProviderGmail, Account: p.account.Alias,
					Address: p.account.Address, Op: op,
					Retryable: true, RetryIn: 30, Err: err,
				}
			}
			return &mail.ProviderError{
				Provider: mail.ProviderGmail, Account: p.account.Alias,
				Address: p.account.Address, Op: op, Err: err,
			}
		case http.StatusTooManyRequests:
			return &mail.ProviderError{
				Provider: mail.ProviderGmail, Account: p.account.Alias,
				Address: p.account.Address, Op: op,
				Retryable: true, RetryIn: 60, Err: err,
			}
		case http.StatusNotFound:
			return mail.ErrNotFound
		case http.StatusBadRequest:
			// Gmail answers 404 for a message that is not there, but 400 invalidArgument for
			// an id it will not parse — and the second is the one a caller actually hits: a
			// draft id used as a message id, an id from another mailbox, an id a model
			// invented. Reported as missing, because an id Gmail refuses to read addresses
			// no message and there is nothing a caller could do differently for the two.
			//
			// Narrow on purpose. A 400 is also what Gmail answers for a request mailroom
			// built wrong, which is a bug here rather than absent mail, and reading every
			// 400 as not-found would turn one into an empty result that looks like an
			// answer. The two id wordings are the whole of it — "Invalid id value" from the
			// single-message routes and "Invalid ids value" from the batch ones.
			if strings.Contains(apiErr.Message, "Invalid id value") ||
				strings.Contains(apiErr.Message, "Invalid ids value") {
				return mail.ErrNotFound
			}
			return &mail.ProviderError{
				Provider: mail.ProviderGmail, Account: p.account.Alias,
				Address: p.account.Address, Op: op, Err: err,
			}
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
			return &mail.ProviderError{
				Provider: mail.ProviderGmail, Account: p.account.Alias,
				Address: p.account.Address, Op: op,
				Retryable: true, Err: err,
			}
		}
	}
	return &mail.ProviderError{
		Provider: mail.ProviderGmail, Account: p.account.Alias,
		Address: p.account.Address, Op: op, Err: err,
	}
}

// transientTokenFailure reports whether a refusal from the token endpoint is one that will
// pass, and how long to wait.
//
// The signal that a refresh token is genuinely finished is invalid_grant, which Google
// documents and returns for a revoked, expired or reissued token. A 429 and a 5xx are the
// endpoint being busy or unwell, and say nothing about the credential.
func transientTokenFailure(err *oauth2.RetrieveError) (retryIn int, transient bool) {
	if err == nil {
		return 0, false
	}
	if err.Response != nil {
		switch {
		case err.Response.StatusCode == http.StatusTooManyRequests:
			return 60, true
		case err.Response.StatusCode >= 500:
			return 30, true
		}
	}
	// Some refusals arrive with no usable response, so the code is the only thing to read.
	// rate_limit_exceeded is not in Google's OAuth error list, but it is what a throttled
	// token request has been seen to carry, and treating it as a dead credential is the
	// failure this whole branch exists to avoid.
	switch err.ErrorCode {
	case "rate_limit_exceeded", "slow_down", "temporarily_unavailable", "server_error":
		return 60, true
	}
	return 0, false
}

// buildQuery renders a canonical Query as Gmail search syntax.
//
// Timestamps use epoch seconds rather than dates. Gmail documents both, and warns about the
// date form in the same breath: "All dates used in the search query are interpreted as
// midnight on that date in the PST timezone. To specify accurate dates for other timezones
// pass the value in seconds instead." A caller's bound is an instant, and rendering it as a
// day in somebody else's timezone would move it by up to a day.
//
// The label is not here. It travels as labelIds on the request itself; see Search.
func buildQuery(q mail.Query) string {
	var parts []string
	if q.Raw != "" {
		parts = append(parts, q.Raw)
	}
	if q.From != "" {
		parts = append(parts, "from:"+quote(q.From))
	}
	if q.To != "" {
		parts = append(parts, "to:"+quote(q.To))
	}
	if q.Subject != "" {
		parts = append(parts, "subject:"+quote(q.Subject))
	}
	if q.Unread {
		parts = append(parts, "is:unread")
	}
	if q.Starred {
		parts = append(parts, "is:starred")
	}
	if q.HasAttach {
		parts = append(parts, "has:attachment")
	}
	if !q.After.IsZero() {
		parts = append(parts, fmt.Sprintf("after:%d", q.After.Unix()))
	}
	if !q.Before.IsZero() {
		parts = append(parts, fmt.Sprintf("before:%d", q.Before.Unix()))
	}
	return strings.Join(parts, " ")
}

// quote renders a search term so Gmail reads it as text rather than as syntax.
//
// A leading hyphen is the one that matters, because it does not fail — it inverts. Gmail
// reads subject:-foo as "subject does not contain foo", so a search for a subject beginning
// with a hyphen answers with the whole mailbox and reports it as a match. Measured against a
// live mailbox: a nonsense term returned nothing, and the same term with a hyphen in front
// returned every message on the page.
//
// A stray double quote is quoted for the same reason in reverse: on its own it can unbalance
// the expression around it. Embedded quotes are dropped rather than escaped, because Gmail's
// query syntax has no escape for them.
//
// Terms are otherwise left bare. Quoting everything would be simpler and would change what
// ordinary searches match — Gmail matches word variants on an unquoted term and an exact
// phrase on a quoted one — so the narrow rule is the one that does not alter results people
// already rely on.
func quote(s string) string {
	if strings.ContainsAny(s, " \t\"") || strings.HasPrefix(s, "-") {
		return `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	return s
}

var _ interface {
	mail.Provider
	mail.MessageReader
	mail.ThreadReader
	mail.AttachmentReader
} = (*Provider)(nil)
