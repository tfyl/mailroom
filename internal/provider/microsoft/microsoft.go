// Package microsoft implements the mail provider interfaces against Microsoft Graph, which
// is how both Microsoft 365 mailboxes and personal outlook.com ones are reached.
//
// Graph rather than IMAP, for two reasons. Basic authentication on IMAP and POP was removed
// from Exchange Online in January 2023 and cannot be re-enabled by anyone, and personal
// outlook.com accounts have been OAuth-only since September 2024 — so an IMAP connector would
// need an OAuth round trip anyway, and would arrive with none of the rest. The rest is the
// point: Graph has server-side search, folders, conversation ids and message rules, which is
// what makes mail_filters and mail_settings mean anything here rather than reporting
// unsupported the way plain IMAP has to.
//
// One connector serves both account kinds because the identity platform's `common` tenant
// accepts both, and because /me/messages is the same endpoint either way. Where the two
// diverge is in what Exchange will answer for a consumer mailbox — message rules and mailbox
// settings are the two that matter — and that divergence is reported per operation rather
// than by withholding the capability, since it is a property of the mailbox and not of the
// provider.
//
// Status: written against the published Graph documentation and passing the static half of
// the conformance suite. Nobody has run it against a live Microsoft mailbox — there is no
// OAuth client registered for one — so every wire-format detail here is documentation rather
// than observation. See docs/providers.md.
package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/tfyl/mailroom/internal/mail"
)

// DefaultBase is the Graph endpoint every call goes to. v1.0 rather than beta: beta changes
// without notice, and a mailbox gateway is a poor place to discover that.
const DefaultBase = "https://graph.microsoft.com/v1.0"

// Scopes requested when linking a Microsoft mailbox.
//
// Fully qualified rather than bare. The identity platform resolves a bare `Mail.ReadWrite`
// against Graph in the ordinary case, but the resource is only implicit — and every scope in
// one authorization request has to belong to one resource, so naming it leaves nothing to
// infer.
//
// Narrow where narrowing is possible. Mail.ReadWrite covers reading, drafting, moving,
// deleting and the categories on a message, so Mail.Read is redundant beside it. Mail.Send is
// separate because Graph separates it, which happens to line up exactly with mailroom
// splitting draft from send. User.Read is read once, at link time, for the mailbox address.
//
// MailboxSettings.ReadWrite is the one worth stating, because guessing it wrong is easy:
// message rules, the automatic-replies setting *and* the master category list are all gated on
// it rather than on Mail.ReadWrite, which grants nothing on any of the three.
//
// offline_access is not optional. Without it the token endpoint returns an access token and
// no refresh token, and the mailbox works only until that token expires — which Microsoft
// deliberately varies between sixty and ninety minutes, so the failure does not even arrive
// at a predictable time.
var Scopes = []string{
	"offline_access",
	"https://graph.microsoft.com/User.Read",
	"https://graph.microsoft.com/Mail.ReadWrite",
	"https://graph.microsoft.com/Mail.Send",
	"https://graph.microsoft.com/MailboxSettings.ReadWrite",
}

// Provider talks to one Microsoft mailbox.
type Provider struct {
	http    *http.Client
	base    string
	account mail.Account

	// bins maps this mailbox's own folder ids to the destructive effect of moving mail into
	// them, resolved on demand because a Graph folder id is opaque. See EffectOfApplying.
	binsMu sync.Mutex
	bins   map[string]mail.LabelEffect
}

func New(ctx context.Context, account mail.Account, source oauth2.TokenSource) (*Provider, error) {
	return &Provider{
		http:    oauth2.NewClient(ctx, source),
		base:    DefaultBase,
		account: account,
	}, nil
}

func (p *Provider) ID() mail.ProviderID { return mail.ProviderMicrosoft }

// Capabilities is derived from the interfaces this type implements, so it cannot drift out
// of step with what the provider actually does.
func (p *Provider) Capabilities() mail.Set { return mail.DerivedCapabilities(p) }

// Quirks warns callers about the two ways Graph differs from Gmail.
//
// Folders are exclusive: a message has one parentFolderId, so applying a folder moves it.
// Categories are not, which is why both are mapped onto Label and told apart by Exclusive —
// the same shape Zoho needs.
//
// Batches are genuinely looped. Graph has a $batch endpoint, but the operations mailroom
// performs on several messages at once are a move and a property patch, and both are
// addressed one message at a time; a caller that knows this can size its own requests
// accordingly instead of assuming a hundred ids cost what one does.
//
// Threading is not derived: conversationId is Exchange's own, so the grouping is
// authoritative and declaring it derived would mislead every agent that reasons about
// conversations.
func (p *Provider) Quirks() []mail.Quirk {
	return []mail.Quirk{mail.QuirkExclusiveLabel, mail.QuirkNoBatch}
}

// --- addressing ---
//
// A Graph message id is unique within the mailbox, so unlike Zoho and IMAP the native part
// of a ScopedID is that id alone. What it is not, by default, is stable: an ordinary Graph id
// encodes the folder, so moving a message — archiving it, or sending a draft — changes it,
// and an id an agent is holding stops resolving.
//
// Immutable ids fix that, and every request here asks for them with the Prefer header below.
// The header has to be asked for consistently: an id minted in one mode is not valid in the
// other, so a single request that forgot it would hand back an id that fails everywhere else.
// Setting it centrally in do is what makes that impossible rather than merely unlikely.
const preferImmutableIDs = `IdType="ImmutableId"`

func (p *Provider) scoped(id string) mail.ScopedID {
	return mail.ScopedID{Account: p.account.ID, Native: id}
}

// escapeID percent-encodes an id for a URL path. Graph ids are base64url-ish and immutable
// ones routinely contain characters a path segment cannot carry raw, so skipping this
// produces requests that 400 or, worse, address a different resource.
func escapeID(id string) string { return url.PathEscape(id) }

// --- transport ---

func (p *Provider) get(ctx context.Context, path string, query url.Values, out any) error {
	return p.do(ctx, http.MethodGet, path, query, nil, out)
}

func (p *Provider) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := p.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return p.request(ctx, method, endpoint, method+" "+path, body, out)
}

// follow fetches an absolute URL Graph handed back, which is how @odata.nextLink paging
// works: the documentation is explicit that a nextLink is opaque and should be followed
// rather than picked apart.
//
// It is still checked against this instance's own base. The URL arrives from the mail
// service over TLS and is no more hostile than a Gmail page token, but a cursor also travels
// out to a client and comes back — so the one place a caller could aim this process at a
// host of their choosing is closed here rather than trusted not to matter.
func (p *Provider) follow(ctx context.Context, absolute string, out any) error {
	if !strings.HasPrefix(absolute, p.base+"/") {
		return fmt.Errorf("malformed cursor: %q does not address this mail service", absolute)
	}
	return p.request(ctx, http.MethodGet, absolute, "next_page", nil, out)
}

func (p *Provider) request(ctx context.Context, method, endpoint, op string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", preferImmutableIDs)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return p.transport(op, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return p.transport(op, err)
	}
	if resp.StatusCode >= 300 {
		failed := decodeFailure(resp.StatusCode, raw)
		if retry := retryAfter(resp); retry > 0 {
			return p.throttled(op, failed, retry)
		}
		return p.wrap(op, failed)
	}
	// A send, a move and a delete all answer 202 or 204 with nothing in the body.
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding the Graph response to %s: %w", op, err)
	}
	return nil
}

// graphError is the envelope Graph answers every failure with. The key is spelled both
// innerError and innererror across Microsoft's own documentation, sometimes on one page, so
// nothing here reads it — the outer code and message are the parts that are consistent.
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// failure is what a non-2xx answer becomes on the way into a typed mailroom error. The status
// and the code travel with it because more than one layer has to look at them: wrap decides
// what kind of error this is, and wrapSettings decides whether a refusal is permanent.
type failure struct {
	status  int
	code    string
	message string
}

func (f *failure) Error() string { return fmt.Sprintf("HTTP %d %s: %s", f.status, f.code, f.message) }

func decodeFailure(status int, raw []byte) *failure {
	f := &failure{status: status, message: snippet(raw)}
	var env graphError
	if err := json.Unmarshal(raw, &env); err == nil {
		f.code = env.Error.Code
		if env.Error.Message != "" {
			f.message = env.Error.Message
		}
	}
	return f
}

// is reports whether a Graph error code matches, ignoring case and the Error prefix Exchange
// puts on the codes it surfaces through Graph.
//
// Matching loosely is not laziness. Graph's error reference publishes no code list for mail at
// all — the only complete list is scoped to OneDrive — and the codes that do turn up here
// arrive in three conventions at once: lowerCamelCase from the Graph gateway, PascalCase from
// its validators, and Error-prefixed from Exchange. Every branch below therefore treats the
// HTTP status as the fact and the code as a hint.
func (f *failure) is(code string) bool {
	if f == nil {
		return false
	}
	got := strings.ToLower(strings.TrimPrefix(f.code, "Error"))
	return got == strings.ToLower(strings.TrimPrefix(code, "Error"))
}

func retryAfter(resp *http.Response) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || seconds < 0 {
		return 0
	}
	return seconds
}

func (p *Provider) throttled(op string, err error, retryIn int) error {
	// The code on a throttle is TooManyRequests from the gateway and ApplicationThrottled when
	// the four-concurrent-request cap is what was hit. Only the second tells an operator
	// anything useful, and it is documented nowhere, so it travels in the message rather than
	// being matched on.
	return &mail.ProviderError{
		Provider: mail.ProviderMicrosoft, Account: p.account.Alias,
		Address: p.account.Address, Op: op,
		Retryable: true, RetryIn: retryIn, Err: err,
	}
}

// wrap turns a Graph failure into one of mailroom's typed errors.
//
// The distinction that matters most is an expired credential: retrying never fixes it and an
// operator has to re-link, so it has to arrive as ErrNeedsReauth — that is what marks the
// mailbox as needing attention rather than leaving a client retrying forever against a
// mailbox that needs a human.
//
// The status is what that turns on, not the code. Graph answers an expired or revoked token
// with a 401, and answers a token that is valid but lacks the scope with a 403 — a different
// problem with a different fix, which must not be folded in here. The code that comes with the
// 401 is InvalidAuthenticationToken, but that string appears in no Graph reference page, so
// nothing depends on it.
func (p *Provider) wrap(op string, failed *failure) error {
	// A message that is not there arrives as a 404, and one whose id no longer addresses
	// anything arrives as a 400 carrying the same code. Both are missing, and a caller that
	// could only see the status would read the second as malformed — which is a different
	// failure with a different fix.
	//
	// InvalidIdMalformed is the third of them, and it is the one a caller actually hits.
	// Graph does not look an id up before parsing it, so anything that is not a well-formed
	// Exchange id — a stale id from another mailbox, an id a model invented, an id truncated
	// in transit — comes back as a 400 ErrorInvalidIdMalformed rather than as either
	// not-found code. Live conformance against a real mailbox on 22 August 2026 is what
	// established that; a stub returning ErrorItemNotFound had covered the two cases Graph
	// answers with least often.
	//
	// It is reported as missing rather than as a distinct malformed error because it is not
	// distinguishable from missing: an id Graph will not parse addresses no message, and
	// there is nothing a caller could do differently for the two.
	if failed.is("ItemNotFound") || failed.is("ResourceNotFound") || failed.is("InvalidIdMalformed") {
		return mail.ErrNotFound
	}

	switch failed.status {
	case http.StatusUnauthorized:
		return mail.ErrNeedsReauth
	case http.StatusNotFound:
		return mail.ErrNotFound
	case http.StatusTooManyRequests:
		// Graph documents Retry-After on a throttle and it is handled before this; a minute is
		// a better guess than telling the caller nothing when the header is missing.
		return p.throttled(op, failed, 60)
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &mail.ProviderError{
			Provider: mail.ProviderMicrosoft, Account: p.account.Alias,
			Address: p.account.Address, Op: op,
			Retryable: true, Err: failed,
		}
	}
	return &mail.ProviderError{
		Provider: mail.ProviderMicrosoft, Account: p.account.Alias,
		Address: p.account.Address, Op: op, Err: failed,
	}
}

// transport reports a request that never reached Graph. Retryable, because a connection that
// failed once may not fail again, and nothing about the mailbox is known to be wrong.
func (p *Provider) transport(op string, err error) error {
	return &mail.ProviderError{
		Provider: mail.ProviderMicrosoft, Account: p.account.Alias,
		Address: p.account.Address, Op: op,
		Retryable: true, Err: err,
	}
}

// unsupported names one operation this mailbox cannot perform, rather than the capability
// containing it. Exchange refuses individual operations on a consumer mailbox that work
// perfectly on a work or school one, and a caller told "settings are unsupported" would stop
// trying the neighbouring calls that do work.
func (p *Provider) unsupported(capability mail.Capability, op, reason string) error {
	return &mail.UnsupportedError{
		Provider:   mail.ProviderMicrosoft,
		Account:    p.account.Alias,
		Address:    p.account.Address,
		Capability: capability,
		Op:         op,
		Reason:     reason,
	}
}

// PrimaryAddress reports the address a set of credentials opens.
//
// Linking needs this before there is anything to build a Provider around: an account row
// wants an address, and the only party who knows it is Microsoft. Nothing is stored yet, so
// the token source is the one the exchange just produced rather than a refreshing one.
func PrimaryAddress(ctx context.Context, source oauth2.TokenSource) (string, error) {
	p := &Provider{
		http: oauth2.NewClient(ctx, source),
		base: DefaultBase,
		// Named rather than left blank because a failure here is reported as a provider
		// error, and those name the mailbox they are about. There is not one yet.
		account: mail.Account{Alias: "the mailbox being linked"},
	}
	me, err := p.me(ctx)
	if err != nil {
		return "", err
	}
	if address := me.address(); address != "" {
		return address, nil
	}
	return "", fmt.Errorf("Microsoft returned no mail address for these credentials")
}

// meResponse is the slice of the user resource linking and send-as need.
//
// ProxyAddresses is returned only when asked for by name, and only for a work or school
// account; a personal Microsoft account has no such collection and leaves it empty. Mail is
// likewise empty on an account with no Exchange mailbox provisioned, which is why
// UserPrincipalName is there to fall back on.
type meResponse struct {
	Mail              string   `json:"mail"`
	UserPrincipalName string   `json:"userPrincipalName"`
	DisplayName       string   `json:"displayName"`
	ProxyAddresses    []string `json:"proxyAddresses"`
}

func (m meResponse) address() string {
	if m.Mail != "" {
		return m.Mail
	}
	// A userPrincipalName is an address in shape and usually in fact. It is the only answer
	// available for an account whose mail property is empty, and a mailbox named by it beats
	// a mailbox named by nothing.
	if strings.Contains(m.UserPrincipalName, "@") {
		return m.UserPrincipalName
	}
	return ""
}

func (p *Provider) me(ctx context.Context) (meResponse, error) {
	query := url.Values{}
	query.Set("$select", "id,mail,userPrincipalName,displayName,proxyAddresses")

	var me meResponse
	if err := p.get(ctx, "/me", query, &me); err != nil {
		return meResponse{}, err
	}
	return me, nil
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
