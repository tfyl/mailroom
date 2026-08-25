// Command mailroom serves the MCP endpoint and the operator interface.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tfyl/mailroom/internal/app"
	"github.com/tfyl/mailroom/internal/auth"
	"github.com/tfyl/mailroom/internal/blob"
	"github.com/tfyl/mailroom/internal/config"
	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mcp"
	"github.com/tfyl/mailroom/internal/notices"
	"github.com/tfyl/mailroom/internal/oauthsrv"
	"github.com/tfyl/mailroom/internal/preflight"
	"github.com/tfyl/mailroom/internal/secrets"
	"github.com/tfyl/mailroom/internal/store"
	"github.com/tfyl/mailroom/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		if err := runSubcommand(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runSubcommand(name string, args []string) error {
	switch name {
	case "invite":
		return runInvite(args)

	case "doctor":
		return runDoctor()

	case "link-imap":
		return runLinkIMAP(args)

	case "generate-key":
		key, err := secrets.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Println(key)
		return nil

	case "version":
		fmt.Println(mcp.Version)
		return nil

	// The runtime image is distroless and has no shell in it, so a file sitting in the image
	// is only reachable by somebody who can reach into the filesystem from outside. Printing
	// the notices from the binary needs nothing but the ability to run the binary, which is
	// the one thing that holds however the image is deployed.
	case "notices":
		fmt.Print(notices.Text)
		return nil

	default:
		return fmt.Errorf("unknown command %q; try link-imap, doctor, invite, generate-key, notices or version", name)
	}
}

// runDoctor checks a deployment against reality rather than against its own configuration.
//
// Worth having because the interesting failures here are invisible from the inside: a
// redirect URI missing from the OAuth client produces an instance that starts cleanly, links
// mailboxes, and cannot be signed into, and nothing on this side can see that. It exits
// non-zero when something is definitely wrong so a deployment can gate on it.
func runDoctor() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var google bool
	for _, p := range cfg.Auth.OIDC {
		if p.ID == "google" {
			google = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	results := preflight.All(ctx, preflight.Deployment{
		PublicURL:             cfg.PublicURL.String(),
		GoogleClientID:        cfg.Google.ClientID,
		GoogleSignIn:          google,
		MicrosoftClientID:     cfg.Microsoft.ClientID,
		MicrosoftClientSecret: cfg.Microsoft.ClientSecret,
		MicrosoftTenant:       cfg.MicrosoftTenant,
		ZohoClientID:          cfg.Zoho.ClientID,
		ZohoRegion:            cfg.ZohoRegion,
	})
	report, problems := preflight.Report(results)
	fmt.Print(report)
	if problems {
		return errors.New("something above needs fixing before this instance will work")
	}
	return nil
}

// runInvite mints an invite from the command line.
//
// Two jobs. With no flags it issues an ordinary invite, which is useful before anybody has
// signed in at all. With --adopt-owner it issues one that moves the account that owns this
// instance onto whichever login redeems it, which is the way back in when the provider that
// issued the original login is gone — a password, before password login was removed.
//
// The authorisation for both is having a shell on the host and the ability to read the
// database, which is strictly more access than the invite grants.
func runInvite(args []string) error {
	flags := flag.NewFlagSet("invite", flag.ContinueOnError)
	adoptOwner := flags.Bool("adopt-owner", false,
		"move the account that owns this instance onto the login that redeems this, instead of creating a new account")
	note := flags.String("note", "", "a note for your own records, shown on the invites page")
	ttl := flags.Duration("expires", 7*24*time.Hour, "how long the invite stays usable; 0 for never")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	users, err := db.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return errors.New("this instance has no accounts yet, so there is nothing to invite " +
			"anybody to and nothing to adopt. The first sign-in claims the instance")
	}
	owner := users[0]

	var code string
	if *adoptOwner {
		_, code, err = db.CreateAdoptionInvite(ctx, owner.ID, *ttl)
	} else {
		_, code, err = db.CreateInvite(ctx, owner.ID, *note, *ttl)
	}
	if err != nil {
		return err
	}

	fmt.Println(cfg.URL("/invite/" + code))
	if *adoptOwner {
		fmt.Fprintf(os.Stderr, "\nOpen that link and sign in. The account %q keeps its "+
			"mailboxes, grants and history, and answers to that login afterwards.\n",
			owner.Display())
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	sealer, err := secrets.NewSealer(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}

	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	operator, err := buildOperatorAuth(ctx, cfg)
	if err != nil {
		return fmt.Errorf("operator authentication: %w", err)
	}

	blobs, err := attachmentStore(cfg, db, log)
	if err != nil {
		return err
	}
	// Bytes on disk outlive the process that wrote them, so the first thing a fresh start
	// does is clear whatever expired while it was down. After that the sweeper is on its own
	// interval; both are the same pass.
	go blobs.SweepEvery(ctx, 5*time.Minute)

	providers := app.NewProviders(db, sealer, cfg)
	gate := grant.NewGate(db, db, db)
	// One queue, shared by both halves of the product: internal/mcp puts a privileged call
	// into it when the grant's mode says it must wait, and internal/web is where its owner
	// answers it. There is no second path into either end.
	holds := held.New(db, providers, db, db, cfg.HeldTTL)
	// The other store of somebody's mail on this volume, and the only one holding messages
	// that exist nowhere else. Same shape as the attachment sweeper above and for the same
	// reason: whatever sat unanswered while the process was down should not survive it.
	go holds.SweepEvery(ctx, held.SweepInterval, log)
	tools := mcp.NewTools(gate, providers, db).
		WithBlobs(blobs).
		WithSendLimit(db, cfg.SendCap.Count, cfg.SendCap.Window).
		WithHoldQueue(holds, cfg.PublicURL.String())

	// The same list forward-auth reads an identity header from. Parsed once here so a bad
	// entry is a startup error rather than a silently ignored line — and parsed even when
	// forward-auth is not configured, because the registration bound needs it either way.
	proxies, err := auth.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return fmt.Errorf("MAILROOM_TRUSTED_PROXIES: %w", err)
	}

	oauthServer := oauthsrv.New(db, cfg.PublicURL.String()).
		WithRegistrationLimit(proxies,
			cfg.RegisterCap.Count, cfg.RegisterCap.Window,
			cfg.RegisterInstanceCap.Count, cfg.RegisterInstanceCap.Window)
	// The third sweeper, and the one with the least at stake: a client registration that
	// never became a grant holds nobody's mail. It is here because the endpoint that writes
	// those rows is the only one on this server a stranger can reach.
	go oauthServer.SweepClientsEvery(ctx, oauthsrv.ClientSweepInterval, cfg.ClientTTL, log)
	mcpServer := mcp.NewServer(oauthServer, tools, cfg.PublicURL.String(), log)

	ui, err := web.New(db, providers, sealer, operator, holds, cfg.Signups, cfg.PublicURL.String(), log)
	if err != nil {
		return err
	}
	oauthServer.ConsentPage = ui.ConsentPage

	mux := http.NewServeMux()
	oauthServer.Routes(mux)
	mcpServer.Routes(mux)
	// Outside the operator guard, like /mcp and for the same reason: whoever fetches or
	// uploads an attachment is a client with no browser session. A signature is the whole
	// authorisation, and it is re-checked against the live grant on every request.
	blob.NewServer(blobs, db, db, log).Routes(mux)
	ui.Routes(mux, oauthServer)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.SecurityHeaders(providers.AuthOrigins(), mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("mailroom listening",
		"addr", cfg.Listen,
		"public_url", cfg.PublicURL.String(),
		"auth_mode", operator.Mode(),
		"signups", cfg.Signups.Mode,
		"mcp_endpoint", cfg.URL("/mcp"),
		"attachment_dir", cfg.Attachments.Dir,
		"attachment_ttl", cfg.Attachments.TTL,
		// Logged rather than warned about. Whether an empty trusted-proxy list is wrong
		// depends on something this process cannot see — whether anything is in front of it
		// — so the honest thing is to say what the bound is keyed on and let the operator
		// recognise their own deployment. Empty behind a proxy means every caller is
		// attributed to the proxy, and the per-address limit becomes a second instance-wide
		// one; see docs/deploying.md.
		"trusted_proxies", len(cfg.TrustedProxies),
		"register_limit", describeRate(cfg.RegisterCap),
		"register_instance_limit", describeRate(cfg.RegisterInstanceCap),
		"client_ttl", cfg.ClientTTL,
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("mailroom stopped")
	return nil
}

// attachmentStore assembles the blob store: bytes on the local disk, metadata in the same
// database as everything else, and a signing key derived from the one secret the operator
// already holds.
//
// Deriving rather than reusing MAILROOM_ENCRYPTION_KEY is the point of the indirection: URL
// signing and credential sealing are different primitives, and one secret meaning two things
// is a secret nobody can reason about. See secrets.Derive.
func attachmentStore(cfg *config.Config, db *store.Store, log *slog.Logger) (*blob.Store, error) {
	key, err := secrets.Derive(cfg.EncryptionKey, blob.SigningPurpose, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving the attachment URL signing key: %w", err)
	}
	signer, err := blob.NewSigner(key)
	if err != nil {
		return nil, err
	}
	dir, err := blob.NewDir(cfg.Attachments.Dir)
	if err != nil {
		return nil, err
	}
	return blob.New(dir, db, signer, cfg.PublicURL.String(), blob.Options{
		TTL:         cfg.Attachments.TTL,
		OwnerQuota:  cfg.Attachments.OwnerQuota,
		InstanceCap: cfg.Attachments.InstanceCap,
	}, log), nil
}

// buildOperatorAuth assembles every configured login method into one registry.
//
// A registry even for a single provider: the alternative is two code paths, one of which is
// exercised far less, and the interesting bugs live in the one nobody runs.
func buildOperatorAuth(ctx context.Context, cfg *config.Config) (*auth.Registry, error) {
	sessions := auth.NewSessions(12 * time.Hour)
	secure := cfg.PublicURL.Scheme == "https"
	registry := auth.NewRegistry(sessions)

	for _, p := range cfg.Auth.OIDC {
		provider, err := auth.NewOIDC(ctx, auth.OIDCOptions{
			ID:            p.ID,
			Label:         p.Label,
			Issuer:        p.Issuer,
			ClientID:      p.ClientID,
			ClientSecret:  p.ClientSecret,
			RedirectURL:   cfg.URL(p.CallbackPath),
			Scopes:        p.Scopes,
			RequiredGroup: p.RequiredGroup,
			RequiredClaim: p.RequiredClaim,
			Sessions:      sessions,
			SecureCookies: secure,
		})
		if err != nil {
			return nil, fmt.Errorf("identity provider %q: %w", p.ID, err)
		}
		registry.AddOIDC(provider)
	}

	if f := cfg.Auth.Forward; f != nil {
		forward, err := auth.NewForward(f.Header, f.TrustedProxies, f.RequiredGroup)
		if err != nil {
			return nil, err
		}
		registry.SetForward(forward)
	}

	if err := registry.Validate(); err != nil {
		return nil, err
	}
	return registry, nil
}

// describeRate renders a configured bound for the startup line, so an operator can read what
// is actually in force rather than what they believe they set.
func describeRate(r config.RateLimit) string {
	if r.Count <= 0 {
		return "off"
	}
	return fmt.Sprintf("%d per %s", r.Count, r.Window)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
