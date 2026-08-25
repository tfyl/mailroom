// Package config loads settings from the environment. Twelve-factor and nothing else: no
// config file format to learn, no secret manager to depend on, and anything that needs one
// can mount a file and export it.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tfyl/mailroom/internal/signup"
)

type AuthMode string

const (
	AuthOIDC    AuthMode = "oidc"
	AuthForward AuthMode = "forward"
)

// googleIssuer lets `google` be named as a provider without configuring an issuer, so
// signing in with Google costs no more than the client credentials an instance that links
// Gmail already has.
const googleIssuer = "https://accounts.google.com"

type Config struct {
	PublicURL     *url.URL
	Listen        string
	DatabaseURL   string
	EncryptionKey string
	LogLevel      string

	Auth    AuthConfig
	Signups signup.Policy

	// Warnings are configuration that loaded but will not do what it looks like it does.
	// Separate from an error because none of it prevents the server running.
	Warnings   []string
	Google     ProviderOAuth
	Zoho       ProviderOAuth
	ZohoRegion string
	Microsoft  ProviderOAuth
	// MicrosoftTenant is the segment the Microsoft identity platform's authorize and token
	// URLs are built around. See where it is read in Load for why it defaults as it does.
	MicrosoftTenant string
	SendCap         RateLimit
	Attachments     AttachmentConfig
	// HeldTTL is how long an action queued by a grant in `hold` mode stays answerable, and
	// so how long the message it holds sits on disk. Zero disables expiry, which is what an
	// instance that would rather lose nothing than lose mail asks for explicitly.
	HeldTTL time.Duration
}

// AttachmentConfig governs the blob store that keeps attachment bytes out of the MCP
// conversation. Every default here is deliberately small: this is a cache of mail that
// already exists elsewhere, sitting on the same volume as the database.
type AttachmentConfig struct {
	// Dir is where blob bytes are written. It defaults beside the database, because that is
	// the path an operator has already made durable and already backs up.
	Dir string
	// TTL is how long bytes stay on disk, and exactly how long a download link lasts. One
	// number rather than two: a blob exists for the fetch it was made for, and a link that
	// outlived its bytes would 404 rather than expire.
	TTL time.Duration
	// OwnerQuota caps one user's share of the store, so a single grant cannot fill the disk
	// the mail database is on. InstanceCap caps every owner together.
	OwnerQuota  int64
	InstanceCap int64
}

// AuthConfig describes every configured way to sign in. Several may be active at once.
//
// There is no password option. Operators authenticate against an identity provider they
// already run or already trust, or against a proxy that has done it for them.
type AuthConfig struct {
	// Mode is the legacy single-provider selector, kept so existing deployments keep
	// working unchanged. Empty when MAILROOM_AUTH_PROVIDERS was used instead.
	Mode AuthMode

	OIDC    []OIDCAuth
	Forward *ForwardAuth
}

type OIDCAuth struct {
	// ID is the url-safe slug in this provider's callback path. It has to be stable: it is
	// half of the redirect URI registered at the issuer.
	ID           string
	Label        string
	Issuer       string
	ClientID     string
	ClientSecret string
	// CallbackPath is where this provider returns to. The legacy single-provider config
	// keeps /auth/callback so an already-registered redirect URI does not have to change.
	CallbackPath  string
	Scopes        []string
	RequiredGroup string
	RequiredClaim string
}

type ForwardAuth struct {
	Header         string
	TrustedProxies []string
	RequiredGroup  string
}

type ProviderOAuth struct {
	ClientID     string
	ClientSecret string
}

func (p ProviderOAuth) Configured() bool { return p.ClientID != "" && p.ClientSecret != "" }

type RateLimit struct {
	Count  int
	Window time.Duration
}

// Load reads configuration and validates it. Validation is deliberately strict at startup:
// every check here is one that would otherwise fail later as a confusing runtime error, or
// worse, not fail at all.
func Load() (*Config, error) {
	c := &Config{
		Listen:      envOr("MAILROOM_LISTEN", ":8080"),
		DatabaseURL: envOr("MAILROOM_DB", "sqlite:///data/mailroom.db"),
		LogLevel:    envOr("MAILROOM_LOG_LEVEL", "info"),
	}

	rawURL := os.Getenv("MAILROOM_PUBLIC_URL")
	if rawURL == "" {
		return nil, fmt.Errorf("MAILROOM_PUBLIC_URL is required: OAuth redirect URIs and the " +
			"discovery documents are derived from it")
	}
	u, err := url.Parse(strings.TrimSuffix(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("MAILROOM_PUBLIC_URL must be an absolute URL, got %q", rawURL)
	}
	c.PublicURL = u

	c.EncryptionKey = os.Getenv("MAILROOM_ENCRYPTION_KEY")
	if c.EncryptionKey == "" {
		return nil, fmt.Errorf("MAILROOM_ENCRYPTION_KEY is required. It seals stored mailbox " +
			"credentials. Generate one with `openssl rand -base64 32` and keep a backup: " +
			"without it every linked mailbox must be re-linked")
	}

	c.Google = ProviderOAuth{
		ClientID:     os.Getenv("MAILROOM_GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("MAILROOM_GOOGLE_CLIENT_SECRET"),
	}
	c.Zoho = ProviderOAuth{
		ClientID:     os.Getenv("MAILROOM_ZOHO_CLIENT_ID"),
		ClientSecret: os.Getenv("MAILROOM_ZOHO_CLIENT_SECRET"),
	}
	// Zoho partitions accounts by data centre, and a mailbox is only reachable through its
	// own region's host. Defaulting to com is right for most, wrong silently for the rest.
	c.ZohoRegion = envOr("MAILROOM_ZOHO_REGION", "com")

	c.Microsoft = ProviderOAuth{
		ClientID:     os.Getenv("MAILROOM_MICROSOFT_CLIENT_ID"),
		ClientSecret: os.Getenv("MAILROOM_MICROSOFT_CLIENT_SECRET"),
	}
	// `common` is the only tenant segment that accepts both a personal Microsoft account and
	// a work or school one, which is the whole point of having a single Microsoft connector:
	// `consumers` refuses every M365 mailbox and `organizations` refuses every outlook.com
	// one. A tenant GUID is the narrower choice for an instance that only ever links its own
	// organisation's mailboxes, so this stays settable.
	c.MicrosoftTenant = envOr("MAILROOM_MICROSOFT_TENANT", "common")

	limit, err := parseRateLimit(envOr("MAILROOM_SEND_RATE_LIMIT", "20/hour"))
	if err != nil {
		return nil, err
	}
	c.SendCap = limit

	if err := c.loadHeldTTL(); err != nil {
		return nil, err
	}

	if err := c.loadAttachments(); err != nil {
		return nil, err
	}

	if err := c.loadAuth(); err != nil {
		return nil, err
	}
	if err := c.loadSignups(); err != nil {
		return nil, err
	}
	if os.Getenv("MAILROOM_REDIS") != "" {
		c.Warnings = append(c.Warnings, "MAILROOM_REDIS is set but does nothing: sessions and "+
			"authorization codes are held in this process, and running more than one replica "+
			"is not supported yet")
	}
	return c, nil
}

// How long an unanswered held action lasts, and the range an operator may move that in.
//
// Three days by default. A held action is a question put to a person who is expected to
// answer it — that is the whole of what `hold` mode is — so the useful lifetime of one is
// measured in hours, and something nobody has looked at after three days is abandoned rather
// than pending. Against that, the row is the only copy of a message that does not exist
// anywhere else, so this is shorter-is-safer up to the point where it starts throwing away
// mail somebody meant to send, and a weekend has to fit inside it.
//
// The ceiling is a month, not a day as it is for attachments, because these are not the same
// object. An attachment copy is mail that already exists in the mailbox and is being cached;
// a held action is mail that exists nowhere else, and expiring it destroys it. An operator
// who wants a long queue is making a defensible trade, and the floor is what stops the
// setting quietly emptying a queue faster than anybody can read it.
const (
	defaultHeldTTL = 72 * time.Hour
	minHeldTTL     = 5 * time.Minute
	maxHeldTTL     = 30 * 24 * time.Hour
)

// loadHeldTTL reads how long a queued action waits before it is reclaimed.
func (c *Config) loadHeldTTL() error {
	raw := os.Getenv("MAILROOM_HELD_TTL")
	// `0` and `off` are the same answer and both are spelled by somebody who meant it. The
	// warning is not decoration: unset, this table is the one place in the database that
	// accumulates message bodies with nothing to bound it, which is the state this setting
	// exists to end.
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "never") {
		c.HeldTTL = 0
		c.Warnings = append(c.Warnings, "MAILROOM_HELD_TTL is off, so an action queued by a "+
			"grant in hold mode waits forever. Each one holds a whole message — recipients, "+
			"body and attachment bytes — unencrypted, and nothing will reclaim one nobody "+
			"answers")
		return nil
	}
	if raw == "" {
		c.HeldTTL = defaultHeldTTL
		return nil
	}

	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("MAILROOM_HELD_TTL must be a duration such as 72h, or `off`, got %q", raw)
	}
	if ttl < minHeldTTL || ttl > maxHeldTTL {
		return fmt.Errorf("MAILROOM_HELD_TTL must be between %s and %s, or `off`; it is how "+
			"long a held action stays answerable and how long the message it holds sits on "+
			"disk", minHeldTTL, maxHeldTTL)
	}
	c.HeldTTL = ttl
	return nil
}

// maxAttachmentTTL is the longest an operator may keep attachment copies.
//
// There is an upper bound at all because the setting is a retention policy on somebody's
// mail, written in an environment variable that is easy to set once and never revisit. A day
// is already far past any legitimate use of a link meant to be fetched immediately, and past
// it the store stops being a buffer and becomes an unindexed second copy of the mailbox.
const maxAttachmentTTL = 24 * time.Hour

// loadAttachments reads where attachment bytes go and how long they live.
func (c *Config) loadAttachments() error {
	dir := os.Getenv("MAILROOM_ATTACHMENT_DIR")
	if dir == "" {
		// Beside the database rather than a fixed /data/attachments: a deployment that moved
		// its database to a different volume moved its data volume, and blobs belong on
		// whichever one that is. It also means a `go run` against ./mailroom.db writes into
		// the checkout instead of failing on a path only the container has.
		dir = filepath.Join(filepath.Dir(strings.TrimPrefix(c.DatabaseURL, "sqlite://")), "attachments")
	}
	c.Attachments.Dir = dir

	ttl, err := time.ParseDuration(envOr("MAILROOM_ATTACHMENT_TTL", "15m"))
	if err != nil {
		return fmt.Errorf("MAILROOM_ATTACHMENT_TTL must be a duration such as 15m, got %q",
			os.Getenv("MAILROOM_ATTACHMENT_TTL"))
	}
	if ttl < time.Minute || ttl > maxAttachmentTTL {
		return fmt.Errorf("MAILROOM_ATTACHMENT_TTL must be between 1m and %s; it is how long "+
			"copies of your mail sit on disk", maxAttachmentTTL)
	}
	c.Attachments.TTL = ttl

	for _, q := range []struct {
		name  string
		def   string
		field *int64
	}{
		{"MAILROOM_ATTACHMENT_QUOTA", "128MiB", &c.Attachments.OwnerQuota},
		{"MAILROOM_ATTACHMENT_CACHE_MAX", "512MiB", &c.Attachments.InstanceCap},
	} {
		size, err := parseSize(envOr(q.name, q.def))
		if err != nil {
			return fmt.Errorf("%s %w", q.name, err)
		}
		*q.field = size
	}
	if c.Attachments.OwnerQuota > c.Attachments.InstanceCap {
		c.Warnings = append(c.Warnings, "MAILROOM_ATTACHMENT_QUOTA is larger than "+
			"MAILROOM_ATTACHMENT_CACHE_MAX, so one user can fill the whole attachment store "+
			"and the per-user quota will never be what refuses them")
	}
	return nil
}

// parseSize reads a byte count written the way an operator would write one.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		scale  int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1000 * 1000 * 1000}, {"MB", 1000 * 1000}, {"KB", 1000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	scale := int64(1)
	for _, u := range units {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			s, scale = strings.TrimSpace(rest), u.scale
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive size such as 512MiB, got %q", s)
	}
	return n * scale, nil
}

// loadSignups reads who may create an account here.
//
// The default is closed. That is the wrong default for an instance behind an identity
// provider that already decides membership, and the right one everywhere else — and of the
// two mistakes, an instance that refuses a colleague until the operator sets one variable is
// far cheaper than one that has been quietly accepting strangers.
func (c *Config) loadSignups() error {
	mode, err := signup.ParseMode(os.Getenv("MAILROOM_SIGNUPS"))
	if err != nil {
		return fmt.Errorf("MAILROOM_SIGNUPS %w", err)
	}
	c.Signups = signup.NewPolicy(mode,
		splitList(os.Getenv("MAILROOM_ALLOWED_EMAILS")),
		splitList(os.Getenv("MAILROOM_ALLOWED_DOMAINS")))

	// An empty allowlist admits nobody, which is what `closed` already says more clearly.
	// Reaching this state means the lists were meant to be set and are not, so failing at
	// startup beats a policy that looks configured and refuses everyone.
	if c.Signups.Mode == signup.Allowlist && len(c.Signups.Emails) == 0 && len(c.Signups.Domains) == 0 {
		return fmt.Errorf("MAILROOM_SIGNUPS=allowlist needs MAILROOM_ALLOWED_EMAILS or " +
			"MAILROOM_ALLOWED_DOMAINS; with neither it would refuse everyone, which is " +
			"MAILROOM_SIGNUPS=closed")
	}
	return nil
}

// loadAuth reads the login configuration.
//
// Two shapes are accepted. MAILROOM_AUTH_PROVIDERS lists several providers by name, which is
// what a shared instance wants; MAILROOM_AUTH_MODE selects exactly one, which is what every
// existing deployment already has. The second is not deprecated — a single-provider instance
// is a perfectly good instance, and breaking those configs to add a feature nobody asked
// them for would be a poor trade.
func (c *Config) loadAuth() error {
	if err := refusePasswordConfig(); err != nil {
		return err
	}
	if names := splitList(os.Getenv("MAILROOM_AUTH_PROVIDERS")); len(names) > 0 {
		return c.loadAuthProviders(names)
	}
	return c.loadAuthMode()
}

// refusePasswordConfig fails startup on configuration for the removed password provider.
//
// Ignoring it would be worse than refusing it. An operator whose MAILROOM_PASSWORD_HASH is
// silently dropped believes they still have a way in, and finds out otherwise at the moment
// they need one — or, if some other provider is also configured, believes the instance is
// protected by a password that is no longer consulted.
func refusePasswordConfig() error {
	for _, name := range []string{"MAILROOM_PASSWORD_HASH", "MAILROOM_TOTP_SECRET"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s is set, but password login has been removed. Configure an "+
				"identity provider instead (MAILROOM_AUTH_PROVIDERS=google is the shortest "+
				"route), then unset this. To move an existing password account onto a new "+
				"login, run `mailroom invite --adopt-owner`", name)
		}
	}
	if strings.EqualFold(os.Getenv("MAILROOM_AUTH_MODE"), "local") {
		return fmt.Errorf("MAILROOM_AUTH_MODE=local is no longer supported: password login " +
			"has been removed. Set MAILROOM_AUTH_PROVIDERS=google, or name any OIDC issuer, " +
			"or use forward-auth behind an authenticating proxy. To move the existing " +
			"account onto a new login, run `mailroom invite --adopt-owner`")
	}
	return nil
}

func (c *Config) loadAuthProviders(names []string) error {
	for _, name := range names {
		switch strings.ToLower(name) {
		case "local":
			return fmt.Errorf("MAILROOM_AUTH_PROVIDERS names \"local\", but password login has " +
				"been removed. Name an OIDC provider such as \"google\", or \"forward\"")

		case "forward":
			forward, err := c.forwardAuth()
			if err != nil {
				return err
			}
			c.Auth.Forward = forward

		default:
			provider, err := c.namedOIDC(name)
			if err != nil {
				return err
			}
			c.Auth.OIDC = append(c.Auth.OIDC, provider)
		}
	}

	if len(c.Auth.OIDC) == 0 && c.Auth.Forward == nil {
		return fmt.Errorf("MAILROOM_AUTH_PROVIDERS named no usable provider")
	}
	return nil
}

// namedOIDC reads one provider out of MAILROOM_OIDC_<NAME>_* variables.
func (c *Config) namedOIDC(name string) (OIDCAuth, error) {
	slug := strings.ToLower(name)
	prefix := "MAILROOM_OIDC_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"

	p := OIDCAuth{
		ID:            slug,
		Label:         envOr(prefix+"LABEL", defaultLabel(slug)),
		Issuer:        os.Getenv(prefix + "ISSUER"),
		ClientID:      os.Getenv(prefix + "CLIENT_ID"),
		ClientSecret:  os.Getenv(prefix + "CLIENT_SECRET"),
		CallbackPath:  "/auth/" + slug + "/callback",
		Scopes:        splitList(os.Getenv(prefix + "SCOPES")),
		RequiredGroup: os.Getenv(prefix + "REQUIRED_GROUP"),
		RequiredClaim: os.Getenv(prefix + "REQUIRED_CLAIM"),
	}
	// Google is known: its issuer is fixed, and the OAuth client that links Gmail mailboxes
	// can sign people in too. Reusing it means an instance already linking Gmail configures
	// nothing further — at the cost of one more redirect URI registered at Google, since the
	// login callback is a different path from the linking one.
	if slug == "google" {
		if p.Issuer == "" {
			p.Issuer = googleIssuer
		}
		if p.ClientID == "" && p.ClientSecret == "" {
			p.ClientID, p.ClientSecret = c.Google.ClientID, c.Google.ClientSecret
		}
	}

	if p.Issuer == "" || p.ClientID == "" || p.ClientSecret == "" {
		if slug == "google" {
			return OIDCAuth{}, fmt.Errorf("signing in with Google needs %sCLIENT_ID and "+
				"%sCLIENT_SECRET, or the MAILROOM_GOOGLE_CLIENT_ID and "+
				"MAILROOM_GOOGLE_CLIENT_SECRET used for linking mailboxes", prefix, prefix)
		}
		return OIDCAuth{}, fmt.Errorf("provider %q needs %sISSUER, %sCLIENT_ID and %sCLIENT_SECRET",
			name, prefix, prefix, prefix)
	}
	return p, nil
}

// defaultLabel gives the login button a name when none was configured. Recognising the
// common issuers is worth the few lines: "Sign in with Google" is what a person expects to
// see, and "Sign in with google" is not.
func defaultLabel(slug string) string {
	switch slug {
	case "google":
		return "Google"
	case "authentik":
		return "Authentik"
	case "keycloak":
		return "Keycloak"
	case "entra", "azure":
		return "Microsoft"
	case "okta":
		return "Okta"
	case "github":
		return "GitHub"
	default:
		return strings.ToUpper(slug[:1]) + slug[1:]
	}
}

func (c *Config) forwardAuth() (*ForwardAuth, error) {
	f := &ForwardAuth{
		Header:         envOr("MAILROOM_FORWARD_HEADER", "X-Forwarded-Email"),
		TrustedProxies: splitList(os.Getenv("MAILROOM_TRUSTED_PROXIES")),
		RequiredGroup:  os.Getenv("MAILROOM_FORWARD_REQUIRED_GROUP"),
	}
	// A trusted identity header is trivially forgeable by anyone who can reach the port
	// directly. Starting without a trusted-proxy list would mean accepting that header from
	// anywhere, so this is a hard failure rather than a warning.
	if len(f.TrustedProxies) == 0 {
		return nil, fmt.Errorf("forward-auth requires MAILROOM_TRUSTED_PROXIES. " +
			"Without it the identity header would be accepted from any source, which any " +
			"client able to reach this port could forge")
	}
	return f, nil
}

// loadAuthMode reads the original single-provider configuration.
func (c *Config) loadAuthMode() error {
	mode := os.Getenv("MAILROOM_AUTH_MODE")
	if mode == "" {
		// No fallback. There is no login method that can be assumed, and starting without
		// one would leave the operator interface either unreachable or unguarded.
		return fmt.Errorf("no login method is configured. Set MAILROOM_AUTH_PROVIDERS to a " +
			"comma-separated list — `google` needs only the OAuth client you already use " +
			"for linking mailboxes, any other name reads MAILROOM_OIDC_<NAME>_ISSUER, " +
			"_CLIENT_ID and _CLIENT_SECRET, and `forward` trusts an authenticating proxy in " +
			"front of this instance")
	}
	c.Auth.Mode = AuthMode(strings.ToLower(mode))

	switch c.Auth.Mode {
	case AuthOIDC:
		p := OIDCAuth{
			ID:    "oidc",
			Label: envOr("MAILROOM_OIDC_LABEL", "Single sign-on"),
			// The single-provider callback keeps its original path, so upgrading does not
			// invalidate a redirect URI already registered at the issuer.
			CallbackPath:  "/auth/callback",
			Issuer:        os.Getenv("MAILROOM_OIDC_ISSUER"),
			ClientID:      os.Getenv("MAILROOM_OIDC_CLIENT_ID"),
			ClientSecret:  os.Getenv("MAILROOM_OIDC_CLIENT_SECRET"),
			Scopes:        splitList(os.Getenv("MAILROOM_OIDC_SCOPES")),
			RequiredGroup: os.Getenv("MAILROOM_OIDC_REQUIRED_GROUP"),
			RequiredClaim: os.Getenv("MAILROOM_OIDC_REQUIRED_CLAIM"),
		}
		if p.Issuer == "" || p.ClientID == "" || p.ClientSecret == "" {
			return fmt.Errorf("MAILROOM_AUTH_MODE=oidc requires MAILROOM_OIDC_ISSUER, " +
				"MAILROOM_OIDC_CLIENT_ID and MAILROOM_OIDC_CLIENT_SECRET")
		}
		c.Auth.OIDC = append(c.Auth.OIDC, p)

	case AuthForward:
		forward, err := c.forwardAuth()
		if err != nil {
			return err
		}
		c.Auth.Forward = forward

	default:
		return fmt.Errorf("MAILROOM_AUTH_MODE must be oidc or forward; got %q", c.Auth.Mode)
	}
	return nil
}

// URL builds an absolute URL under the public base.
func (c *Config) URL(path string) string {
	return strings.TrimSuffix(c.PublicURL.String(), "/") + path
}

func parseRateLimit(s string) (RateLimit, error) {
	count, window, found := strings.Cut(s, "/")
	if !found {
		return RateLimit{}, fmt.Errorf("rate limit must look like 20/hour, got %q", s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || n <= 0 {
		return RateLimit{}, fmt.Errorf("rate limit count must be a positive integer, got %q", count)
	}
	var d time.Duration
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "minute", "min", "m":
		d = time.Minute
	case "hour", "hr", "h":
		d = time.Hour
	case "day", "d":
		d = 24 * time.Hour
	default:
		return RateLimit{}, fmt.Errorf("rate limit window must be minute, hour or day; got %q", window)
	}
	return RateLimit{Count: n, Window: d}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
