package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/mail"
	"github.com/tfyl/mailroom/internal/provider/gmail"
	imapprovider "github.com/tfyl/mailroom/internal/provider/imap"
	"github.com/tfyl/mailroom/internal/provider/microsoft"
	"github.com/tfyl/mailroom/internal/provider/zoho"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/store"
)

// GmailScopes is the full set requested when linking a Google mailbox.
//
// Google *replaces* granted scopes on re-consent rather than merging them, so a partial
// request silently drops the rest and surfaces much later as a 403 on an unrelated call.
// Operators who never intend to send should remove the send scope here rather than hoping a
// narrower login will stick.
var GmailScopes = []string{
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/gmail.compose",
	"https://www.googleapis.com/auth/gmail.send",
	"https://www.googleapis.com/auth/gmail.settings.basic",
	"https://www.googleapis.com/auth/userinfo.email",
	"openid",
}

// Providers builds live provider clients for linked accounts, unsealing stored credentials
// on the way.
type Providers struct {
	store  *store.Store
	sealer *secrets.Sealer
	google *oauth2.Config

	zoho       *oauth2.Config
	zohoRegion zoho.Region

	microsoft *oauth2.Config

	mu    sync.Mutex
	cache map[mail.AccountID]cached

	grace time.Duration
}

type cached struct {
	provider mail.Provider
	built    time.Time
}

// cacheTTL bounds how long a provider client is reused. Short enough that a re-link or a
// revocation takes effect promptly, long enough that a burst of tool calls does not rebuild
// an HTTP client each time.
const cacheTTL = 5 * time.Minute

// releaseGrace is how long a provider displaced from the cache is left alive before whatever
// it holds open is closed.
//
// A caller is handed the provider itself and has nowhere to give it back: the tool layer
// type-asserts against it to find which capabilities it implements, so it cannot be passed a
// wrapper that counts references, and the interface has no release. The cache is therefore
// the only thing that knows a provider has become unreachable, and a delay is the only way
// to let a call already using one finish. Generous on purpose — being late costs one idle
// connection for a few minutes, being early costs somebody a failed request.
const releaseGrace = 2 * time.Minute

func NewProviders(st *store.Store, sealer *secrets.Sealer, cfg *config.Config) *Providers {
	p := &Providers{store: st, sealer: sealer, cache: map[mail.AccountID]cached{}, grace: releaseGrace}
	if cfg.Google.Configured() {
		p.google = &oauth2.Config{
			ClientID:     cfg.Google.ClientID,
			ClientSecret: cfg.Google.ClientSecret,
			Endpoint:     googleoauth.Endpoint,
			RedirectURL:  cfg.URL("/accounts/link/google/callback"),
			Scopes:       GmailScopes,
		}
	}
	if cfg.Zoho.Configured() {
		p.zoho = &oauth2.Config{
			ClientID:     cfg.Zoho.ClientID,
			ClientSecret: cfg.Zoho.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.zoho." + string(cfg.ZohoRegion) + "/oauth/v2/auth",
				TokenURL: "https://accounts.zoho." + string(cfg.ZohoRegion) + "/oauth/v2/token",
				// Zoho documents the client id and secret as request parameters, never as
				// Basic credentials. Left unset the library probes with a Basic-auth request
				// first and falls back after it fails, which works but spends a round trip
				// being refused on every single token exchange and refresh.
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.URL("/accounts/link/zoho/callback"),
			Scopes:      zoho.Scopes,
		}
		p.zohoRegion = zoho.Region(cfg.ZohoRegion)
	}
	if cfg.Microsoft.Configured() {
		tenant := cfg.MicrosoftTenant
		if tenant == "" {
			tenant = "common"
		}
		p.microsoft = &oauth2.Config{
			ClientID:     cfg.Microsoft.ClientID,
			ClientSecret: cfg.Microsoft.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/authorize",
				TokenURL: "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token",
				// The Microsoft identity platform documents the client credentials as form
				// parameters on the token request. Left unset the library probes with a
				// Basic-auth request first and falls back after it is refused, which spends a
				// round trip being told no on every exchange and every refresh.
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.URL("/accounts/link/microsoft/callback"),
			Scopes:      microsoft.Scopes,
		}
	}
	return p
}

// MicrosoftOAuth returns the configured Microsoft client, or nil when Microsoft linking is
// not set up on this instance.
func (p *Providers) MicrosoftOAuth() *oauth2.Config { return p.microsoft }

// ZohoOAuth returns the configured Zoho client, or nil when Zoho linking is not set up.
func (p *Providers) ZohoOAuth() *oauth2.Config { return p.zoho }

// ZohoRegion reports the data centre Zoho linking is configured against. Linking needs it
// separately from the OAuth client: consent is granted at accounts.zoho.<region> while the
// mailbox itself lives at mail.zoho.<region>, and the address is read from the second.
func (p *Providers) ZohoRegion() zoho.Region { return p.zohoRegion }

// AuthOrigins lists the origins a linking flow hands the browser off to, for the
// Content-Security-Policy.
//
// Linking posts a form to mailroom, which answers with a redirect to the provider's consent
// screen — and browsers apply `form-action` to the whole redirect chain that follows a form
// submission, not just its immediate target. So a policy of `form-action 'self'` blocks the
// handoff, and does it at the browser with no server-side trace.
//
// Derived from the configured providers rather than hardcoded: Zoho's host varies by data
// centre, and an instance with no provider configured should not be advertising extra
// origins it never uses.
func (p *Providers) AuthOrigins() []string {
	var out []string
	for _, conf := range []*oauth2.Config{p.google, p.zoho, p.microsoft} {
		if conf == nil || conf.Endpoint.AuthURL == "" {
			continue
		}
		if u, err := url.Parse(conf.Endpoint.AuthURL); err == nil && u.Scheme != "" && u.Host != "" {
			out = append(out, u.Scheme+"://"+u.Host)
		}
	}
	return out
}

// GoogleOAuth returns the configured Google client, or nil when Google linking is not set up.
func (p *Providers) GoogleOAuth() *oauth2.Config { return p.google }

func (p *Providers) For(ctx context.Context, acct mail.Account) (mail.Provider, error) {
	p.mu.Lock()
	if c, ok := p.cache[acct.ID]; ok && time.Since(c.built) < cacheTTL {
		p.mu.Unlock()
		return c.provider, nil
	}
	p.mu.Unlock()

	built, err := p.build(ctx, acct)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	displaced := p.cache[acct.ID].provider
	p.cache[acct.ID] = cached{provider: built, built: time.Now()}
	p.mu.Unlock()

	p.release(displaced)
	return built, nil
}

// Forget drops a cached client, so a re-link takes effect without waiting out the TTL.
func (p *Providers) Forget(id mail.AccountID) {
	p.mu.Lock()
	displaced := p.cache[id].provider
	delete(p.cache, id)
	p.mu.Unlock()

	p.release(displaced)
}

// release closes a provider nothing can reach any more.
//
// Only the providers holding a connection implement io.Closer. Gmail and Zoho are HTTP
// clients with nothing to release, and an unclosed IMAP connection is a socket on both ends
// until a server times it out — Gmail allows fifteen at once per account and Dovecot ten per
// address, so a mailbox rebuilt every five minutes stops being reachable within the hour.
func (p *Providers) release(prov mail.Provider) {
	closer, ok := prov.(io.Closer)
	if !ok {
		return
	}
	time.AfterFunc(p.grace, func() { _ = closer.Close() })
}

// refreshing builds the token source a provider talks to, wrapping the ordinary OAuth one so
// that a rotated refresh token is written back rather than dropped.
func (p *Providers) refreshing(ctx context.Context, conf *oauth2.Config, acct mail.Account, refresh string) oauth2.TokenSource {
	return &rotatingSource{
		base:    conf.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}),
		store:   p.store,
		sealer:  p.sealer,
		account: acct,
		ctx:     ctx,
		current: refresh,
	}
}

// rotatingSource stores a refresh token that has changed.
//
// A refresh token need not be the one it was issued as: an authorisation server may return a
// new one with each refresh and invalidate its predecessor. Google rarely does, which is why
// nothing here noticed — but rotation is increasingly the default elsewhere, and against a
// provider that rotates, a mailbox works exactly until the token in the database is used a
// second time and then fails as a credential error with nothing to point at.
type rotatingSource struct {
	base    oauth2.TokenSource
	store   *store.Store
	sealer  *secrets.Sealer
	account mail.Account

	// ctx is the detached one build made, never a request's. A refresh happens during
	// whichever call happens to need a token, and that call's context is cancelled the moment
	// it returns — a write racing that is worse than no write at all.
	ctx context.Context

	// current is the token the store holds, so a refresh that returns the same one writes
	// nothing. Providers are cached and shared, so this is read from several calls at once.
	mu      sync.Mutex
	current string
}

func (s *rotatingSource) Token() (*oauth2.Token, error) {
	token, err := s.base.Token()
	if err != nil || token == nil || token.RefreshToken == "" {
		return token, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if token.RefreshToken == s.current {
		return token, nil
	}

	if err := s.persist(token.RefreshToken); err != nil {
		// The token in hand is good, so the call that asked for it goes ahead. current is
		// left as it was, so the next refresh tries the write again rather than assuming it
		// happened.
		slog.Default().Error("storing a rotated refresh token failed; this mailbox will need re-linking if the provider has invalidated the old one",
			"account", s.account.ID, "alias", s.account.Alias, "error", err)
		return token, nil
	}
	s.current = token.RefreshToken
	return token, nil
}

// persist seals under the account id, which is the additional authenticated data the
// credential column was written with. Sealing without it produces a row that unseals nowhere.
func (s *rotatingSource) persist(refresh string) error {
	sealed, err := s.sealer.SealString(refresh, string(s.account.ID))
	if err != nil {
		return err
	}
	return s.store.UpdateCredential(s.ctx, s.account.OwnerID, s.account.ID, sealed)
}

// build constructs a live client for an account.
//
// The context deliberately is *not* the caller's. These clients are cached and reused across
// requests, and an OAuth token source refreshes lazily — so a client built with a request
// context keeps that context internally and every refresh after that request finishes fails
// with "context canceled". The symptom is a provider that works once and then stops, which
// is a miserable thing to chase. Per-call deadlines still apply: every API call passes the
// caller's context explicitly.
func (p *Providers) build(_ context.Context, acct mail.Account) (mail.Provider, error) {
	ctx := context.WithoutCancel(context.Background())

	sealed, err := p.store.Credential(ctx, acct.OwnerID, acct.ID)
	if err != nil {
		return nil, err
	}
	// The account id is bound in as additional authenticated data, so a credential row copied
	// between accounts fails to open rather than silently authorising the wrong mailbox.
	refresh, err := p.sealer.OpenString(sealed, string(acct.ID))
	if err != nil {
		return nil, fmt.Errorf("credential for %s could not be decrypted: %w", acct.Alias, err)
	}

	switch acct.Provider {
	case mail.ProviderGmail:
		if p.google == nil {
			return nil, fmt.Errorf("Google linking is not configured on this instance")
		}
		return gmail.New(ctx, acct, p.refreshing(ctx, p.google, acct, refresh))

	case mail.ProviderIMAP:
		// IMAP has no OAuth. The sealed credential is the whole connection description —
		// host, username, password — rather than a refresh token, which is why the
		// credential column stores an opaque string instead of a token type.
		var cfg imapprovider.Config
		if err := json.Unmarshal([]byte(refresh), &cfg); err != nil {
			return nil, fmt.Errorf("stored IMAP credential for %s is not readable: %w", acct.Alias, err)
		}
		return imapprovider.New(ctx, acct, cfg)

	case mail.ProviderZoho:
		if p.zoho == nil {
			return nil, fmt.Errorf("Zoho linking is not configured on this instance")
		}
		return zoho.New(ctx, acct, p.refreshing(ctx, p.zoho, acct, refresh), zoho.Options{Region: p.zohoRegion})

	case mail.ProviderMicrosoft:
		if p.microsoft == nil {
			return nil, fmt.Errorf("Microsoft linking is not configured on this instance")
		}
		return microsoft.New(ctx, acct, p.refreshing(ctx, p.microsoft, acct, refresh))
	default:
		return nil, fmt.Errorf("provider %q is not implemented yet", acct.Provider)
	}
}
