// Package zoho implements the mail provider interfaces against the Zoho Mail API.
//
// Zoho differs from Gmail in the two ways that most stress the abstraction, which is why it
// is the second provider: pagination is offset-based rather than cursor-based, and a message
// is addressed by folder *and* id rather than by id alone. Both are absorbed here so the
// tool layer needs no provider branches.
//
// Status: passing both halves of the conformance suite, the behavioural half against a real
// Zoho mailbox. That run is where the wire-format notes in this package come from — where a
// comment here says what Zoho answered, it was observed rather than read. See
// docs/providers.md for what it turned up and what it left unproven.
package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/tfyl/mailroom/internal/mail"
)

// Region determines the API host. Zoho partitions accounts by data centre and an account is
// only reachable through its own region's host.
type Region string

const (
	RegionUS Region = "com"
	RegionEU Region = "eu"
	RegionIN Region = "in"
	RegionAU Region = "com.au"
	RegionJP Region = "jp"
)

// Scopes requested when linking a Zoho mailbox.
//
// accounts.UPDATE is here for one call: switching the vacation reply off, which Zoho puts on
// the account record rather than in a settings API. Everything else on the account is read
// only, and accounts.READ covers it. A mailbox linked before this was asked for holds a token
// without it, so that one call will be refused until the mailbox is linked again — which is a
// re-link for a setting, not for the mail.
var Scopes = []string{
	"ZohoMail.messages.ALL",
	"ZohoMail.folders.ALL",
	"ZohoMail.tags.ALL",
	"ZohoMail.accounts.READ",
	"ZohoMail.accounts.UPDATE",
}

// Provider talks to one Zoho mailbox.
type Provider struct {
	http      *http.Client
	base      string
	accountID string // Zoho's own account id, distinct from mailroom's
	account   mail.Account

	// folders maps folder id to folder name, read once and kept, because classifying a folder
	// as the bin needs its name and a numeric id does not carry one. See EffectOfApplying.
	foldersMu sync.Mutex
	folders   map[string]string
}

type Options struct {
	Region Region
	// ZohoAccountID is the id Zoho assigns the mailbox, discovered via /api/accounts.
	ZohoAccountID string
}

func New(ctx context.Context, account mail.Account, source oauth2.TokenSource, opts Options) (*Provider, error) {
	region := opts.Region
	if region == "" {
		region = RegionUS
	}
	p := &Provider{
		http:      newClient(ctx, source),
		base:      fmt.Sprintf("https://mail.zoho.%s/api", region),
		accountID: opts.ZohoAccountID,
		account:   account,
	}
	if p.accountID == "" {
		id, err := p.discoverAccountID(ctx)
		if err != nil {
			return nil, err
		}
		p.accountID = id
	}
	return p, nil
}

func (p *Provider) ID() mail.ProviderID { return mail.ProviderZoho }

// Capabilities is derived from the interfaces this type implements, so it cannot drift out
// of step with what the provider actually does.
func (p *Provider) Capabilities() mail.Set { return mail.DerivedCapabilities(p) }

// Quirks warns callers about the way Zoho behaves differently from Gmail.
//
// Folders are exclusive, so applying one moves the message.
//
// Threading is derived, which reverses what this said. Zoho does thread mail, and
// /messages/view?threadId=<thread> does return a conversation's members — but nothing tells
// mailroom what <thread> is for a message it has just listed.
//
// The listing and the search endpoint both answer without a threadId field at all. Zoho
// reports one only when threadedMails=true, and that parameter is a filter rather than an
// annotation: Zoho documents it as retrieving "emails that are a part of conversations", and
// on the live mailbox it cut one folder's first 200 messages to 4 and the inbox's to 137. A
// listing that hides most of the mailbox cannot be the one Search pages through, and
// /messages/search refuses the parameter outright ("threadedMails Extra paramters given").
//
// So mailroom reaches a thread by treating a message's own id as its thread id. That is true
// for the message that started a thread and false for every reply — asking Zoho for a reply's
// own id as a thread returns nothing. The grouping is therefore an inference, and this quirk
// is how a caller learns not to read a one-message answer as proof there were no replies.
//
// Batches are not looped either, though this claimed they were. updatemessage takes every
// message id in one request, so a client told otherwise splits work it could have sent
// whole — the warning cost throughput and bought nothing.
func (p *Provider) Quirks() []mail.Quirk {
	return []mail.Quirk{mail.QuirkExclusiveLabel, mail.QuirkDerivedThreads, mail.QuirkUnstablePaging}
}

// Zoho has no star. Its nearest equivalent is the flag set, so starred maps onto that flag
// in every direction at once: SetFlags writes it, Search filters on it, and a message
// carrying it is reported starred. A mapping used by one of the three and not the others is
// how a filter comes back with results that then say they are not starred.
//
// Zoho spells the same flag two ways, and which one is right depends on the direction. The
// listing endpoint's flagid parameter is an integer — 0 flag_not_set, 1 info, 2 important,
// 3 followup — while the update endpoint's setFlag mode takes the name. Zoho's own response
// samples use both: the listing sample answers "flag_not_set" and the search sample answers
// 2, on adjacent pages. So a flag is written by name, filtered on by number, and read as
// whichever of the two arrived.
const (
	flagFollowUp = 3
	flagNone     = 0

	flagNameFollowUp = "followup"
	flagNameNone     = "flag_not_set"
)

// --- addressing ---
//
// Reading a message needs its folder as well as its id, so the native part of a ScopedID
// carries both. Callers never parse it; it is opaque above this package, which is exactly
// what lets one provider need two identifiers and another need one.

func nativeID(folderID, messageID string) string { return folderID + "/" + messageID }

func splitNative(native string) (folderID, messageID string, err error) {
	folderID, messageID, ok := strings.Cut(native, "/")
	if !ok || folderID == "" || messageID == "" {
		return "", "", fmt.Errorf("malformed zoho message id %q: want <folder>/<message>", native)
	}
	return folderID, messageID, nil
}

func (p *Provider) scoped(folderID, messageID string) mail.ScopedID {
	return mail.ScopedID{Account: p.account.ID, Native: nativeID(folderID, messageID)}
}

// newClient builds the HTTP client every Zoho call goes through.
//
// Not oauth2.NewClient, because Zoho does not accept the scheme its own token endpoint names.
// Zoho answers with `"token_type": "Bearer"` and then refuses `Authorization: Bearer <token>`
// on every Mail API endpoint: the header it wants is `Zoho-oauthtoken <token>`. The standard
// client takes the scheme from the token type and cannot be told otherwise, so the header is
// written here instead.
func newClient(ctx context.Context, source oauth2.TokenSource) *http.Client {
	// With no source, this hands back whatever client the context carries and http.DefaultClient
	// otherwise — which is how a test substitutes a transport for the whole flow.
	base := oauth2.NewClient(ctx, nil).Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: &authTransport{base: base, source: source}}
}

type authTransport struct {
	base   http.RoundTripper
	source oauth2.TokenSource
}

func (a *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token, err := a.source.Token()
	if err != nil {
		return nil, err
	}
	// A RoundTripper may not modify the request it is handed, and the caller may still be
	// holding it.
	cloned := r.Clone(r.Context())
	cloned.Header.Set("Authorization", "Zoho-oauthtoken "+token.AccessToken)
	return a.base.RoundTrip(cloned)
}

// --- identifiers on the wire ---

// flexString holds a Zoho identifier, which Zoho spells as a JSON string on one endpoint and
// as a bare JSON number on another.
//
// Observed against the live mailbox: /messages/view, /messages/search and
// /folders/{f}/messages/{m}/details all answer `"messageId":"1234567890123456789"`, while
// /folders/{f}/messages/{m}/content and .../header answer the same id as
// `"messageId":1234567890123456789`. A struct that decodes only the string form fails on
// content with "cannot unmarshal number into Go struct field", which is every id search hands
// back becoming unopenable. Zoho's own published samples disagree the same way and disagree
// again about where: the search sample shows both messageId and threadId unquoted, the listing
// sample shows both quoted. Neither endpoint can be relied on to pick one, so nothing here
// depends on which arrives.
//
// The digits are kept as they arrived rather than being decoded as a number. Zoho's ids are
// 19 digits and a float64 carries 15 to 16 of them, so a round trip through the default
// numeric decoding turns 1234567890123456789 into 1234567890123456768 — an id that resolves
// to nothing, which is a worse failure than the decode error because it looks like it worked.
type flexString string

func (f flexString) String() string { return string(f) }

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0, bytes.Equal(b, []byte("null")):
		*f = ""
		return nil
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	case b[0] == '{' || b[0] == '[':
		// A third shape is a change in the response, not a spelling of the same value.
		// Guessing at one would put whatever it is into an id.
		return fmt.Errorf("zoho identifier is neither a string nor a number: %s", snippet(b))
	default:
		*f = flexString(b)
		return nil
	}
}

// --- transport ---

// envelope is Zoho's uniform response wrapper.
type envelope struct {
	Status struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"status"`
	Data json.RawMessage `json:"data"`
}

// apiError is the failure half of that wrapper's data object.
//
// ErrorCode is the field that matters. Zoho sets it on every request it could not parse —
// EXTRA_PARAM_FOUND for a parameter the endpoint does not take, DATATYPE_NOT_MATCHED for a
// value of the wrong type, MORE_THAN_MAX_OCCURANCE for a repeated parameter,
// URL_RULE_NOT_CONFIGURED for a path segment that does not match the route — and leaves it
// unset when the request was well formed and the thing it named does not exist.
type apiError struct {
	ErrorCode string `json:"errorCode"`
	MoreInfo  string `json:"moreInfo"`
}

func (p *Provider) get(ctx context.Context, path string, query url.Values, out any) error {
	return p.do(ctx, http.MethodGet, path, query, nil, out)
}

func (p *Provider) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := p.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return p.wrap(method+" "+path, 0, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return p.wrap(method+" "+path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 {
		if isMissingMessage(resp.StatusCode, raw, path) {
			return mail.ErrNotFound
		}
		return p.wrap(method+" "+path, resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, snippet(raw)))
	}
	if out == nil {
		return nil
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decoding zoho response: %w", err)
	}
	// Any 2xx is success. Zoho's envelope carries an HTTP-shaped code of its own, and it is
	// not always 200: creating a label answers 201 with the description "Created", which this
	// read as a failure — so CreateLabel returned an error having successfully created the
	// label, leaving one behind on every call. The envelope is checked here as well as
	// against the response status because the two disagree: an endpoint can answer HTTP 200
	// with a failing envelope, and that has to be caught.
	if env.Status.Code != 0 && env.Status.Code >= 300 {
		if isMissingMessage(env.Status.Code, raw, path) {
			return mail.ErrNotFound
		}
		return p.wrap(method+" "+path, env.Status.Code, fmt.Errorf("%s", env.Status.Description))
	}
	if len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// Zoho has two wordings for a message that is not there, and which one arrives depends on the
// endpoint. /content, /header and /attachmentinfo name the id back — "Message id <id> is
// invalid". /details never does, whatever id it is given: it says only "messageId is invalid".
// Both were observed against the live mailbox on the same absent id.
var (
	missingMessageNamed = regexp.MustCompile(`^Message id (\S+) is invalid$`)
	missingMessageBare  = regexp.MustCompile(`^messageId is invalid$`)
	messageIDSegment    = regexp.MustCompile(`^\d+$`)
)

// isMissingMessage decides whether a Zoho refusal means "no such message".
//
// Zoho answers 400 Invalid Input where the other three providers answer 404, so without this
// a caller cannot tell a message that has been deleted from a mailbox it cannot reach. But
// 400 is also what Zoho answers for a request it could not parse, which is a bug in mailroom
// rather than absent mail — this provider has shipped two of those — and reading every 400 as
// not-found would have turned both into an empty result that looked like an answer. So the
// test is narrow, and each part of it was measured rather than assumed:
//
//   - The status is 400. A malformed path — a non-numeric id, an id too long for the route —
//     never reaches this code: Zoho rejects those at 404 URL_RULE_NOT_CONFIGURED.
//   - Zoho set no errorCode. It sets one on every request it could not parse
//     (EXTRA_PARAM_FOUND, DATATYPE_NOT_MATCHED, MORE_THAN_MAX_OCCURANCE) and on no absent
//     message, so the discrimination is Zoho's own rather than mailroom's reading of prose.
//     This is the condition carrying the weight; the two below stop it reaching too far.
//   - moreInfo is one of the two sentences above and nothing else. A request malformed in some
//     other way says what was wrong with it instead — "flagid Invalid data type".
//   - The request addressed one message by id: the path has a numeric segment after
//     /messages/, and where Zoho named an id, it is that one. A listing, a search or a batch
//     update cannot satisfy this, so a 400 from any of them stays a provider error however it
//     is worded.
//
// Anything short of all four is reported as it was, because a refusal mailroom has
// misunderstood should look like a failure rather than like mail that is not there.
func isMissingMessage(status int, raw []byte, path string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var env struct {
		Data apiError `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	if env.Data.ErrorCode != "" {
		return false
	}

	addressed := addressedMessageID(path)
	if addressed == "" {
		return false
	}

	info := strings.TrimSpace(env.Data.MoreInfo)
	if missingMessageBare.MatchString(info) {
		return true
	}
	match := missingMessageNamed.FindStringSubmatch(info)
	return match != nil && match[1] == addressed
}

// addressedMessageID reports the message a request names, or "" when it names none. The shape
// is /accounts/{account}/folders/{folder}/messages/{message}/…, and "view" and "search" sit in
// the same position on the listing endpoints — which is why the segment has to be numeric to
// count.
func addressedMessageID(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment != "messages" || i+1 >= len(segments) {
			continue
		}
		if candidate := segments[i+1]; messageIDSegment.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

// wrap turns a Zoho failure into one of mailroom's typed errors.
//
// The distinction that matters most is an expired credential: retrying never fixes it and an
// operator has to re-link, so reporting it as a generic failure would leave a client
// retrying forever against a mailbox that needs a human.
func (p *Provider) wrap(op string, status int, err error) error {
	// A refusal from the token endpoint arrives with no HTTP status of its own, so it is
	// read before the switch or it falls through to a generic failure — which is what used
	// to happen, in both directions. A dead refresh token was never reported as needing a
	// re-link, so the mailbox stayed marked healthy and every call failed obscurely; and a
	// throttle was reported as permanent, so a client had no reason to wait.
	//
	// Zoho rate-limits the token endpoint in earnest — "You have made too many requests
	// continuously. Please try again after some time." — under an errorCode of Access
	// Denied, which is why the description is read rather than the code alone.
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		body := strings.ToLower(string(retrieveErr.Body) + " " + retrieveErr.ErrorDescription)
		switch {
		case strings.Contains(body, "too many requests"),
			retrieveErr.Response != nil && retrieveErr.Response.StatusCode == http.StatusTooManyRequests,
			retrieveErr.Response != nil && retrieveErr.Response.StatusCode >= 500:
			return &mail.ProviderError{
				Provider: mail.ProviderZoho, Account: p.account.Alias,
				Address: p.account.Address, Op: op,
				Retryable: true, RetryIn: 60, Err: err,
			}
		case strings.Contains(body, "invalid_grant"), strings.Contains(body, "invalid_client"):
			return mail.ErrNeedsReauth
		}
	}

	switch status {
	case http.StatusUnauthorized:
		return mail.ErrNeedsReauth
	case http.StatusNotFound:
		return mail.ErrNotFound
	case http.StatusTooManyRequests:
		return &mail.ProviderError{
			Provider: mail.ProviderZoho, Account: p.account.Alias,
			Address: p.account.Address, Op: op,
			Retryable: true, RetryIn: 60, Err: err,
		}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return &mail.ProviderError{
			Provider: mail.ProviderZoho, Account: p.account.Alias,
			Address: p.account.Address, Op: op,
			Retryable: true, Err: err,
		}
	}
	return &mail.ProviderError{
		Provider: mail.ProviderZoho, Account: p.account.Alias,
		Address: p.account.Address, Op: op, Err: err,
	}
}

// PrimaryAddress reports the address of the mailbox a set of credentials opens.
//
// Linking needs this before there is anything to build a Provider around: an account row
// wants an address, and the only party who knows it is Zoho. Nothing here is stored yet, so
// the token source is the one the exchange just produced rather than a refreshing one.
func PrimaryAddress(ctx context.Context, region Region, source oauth2.TokenSource) (string, error) {
	if region == "" {
		region = RegionUS
	}
	p := &Provider{
		http: newClient(ctx, source),
		base: fmt.Sprintf("https://mail.zoho.%s/api", region),
		// Named rather than left blank because a failure here is reported as a provider error,
		// and those name the mailbox they are about. There is not one yet.
		account: mail.Account{Alias: "the mailbox being linked"},
	}

	var accounts []struct {
		PrimaryMail string `json:"primaryEmailAddress"`
	}
	if err := p.get(ctx, "/accounts", nil, &accounts); err != nil {
		return "", err
	}
	for _, a := range accounts {
		if a.PrimaryMail != "" {
			return a.PrimaryMail, nil
		}
	}
	return "", fmt.Errorf("zoho returned no mail address for these credentials")
}

func (p *Provider) discoverAccountID(ctx context.Context) (string, error) {
	var accounts []struct {
		AccountID   string `json:"accountId"`
		PrimaryMail string `json:"primaryEmailAddress"`
	}
	if err := p.get(ctx, "/accounts", nil, &accounts); err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("zoho returned no mail accounts for these credentials")
	}
	// Prefer the account matching the address mailroom recorded, so a login holding several
	// mailboxes cannot silently bind to the wrong one.
	for _, a := range accounts {
		if strings.EqualFold(a.PrimaryMail, p.account.Address) {
			return a.AccountID, nil
		}
	}
	return accounts[0].AccountID, nil
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// zohoTime parses Zoho's millisecond epoch timestamps, which arrive as either a number or a
// quoted string depending on the endpoint.
func zohoTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	s := strings.Trim(string(raw), `"`)
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
