package config

import (
	"os"
	"strings"
	"testing"
)

// base sets the two variables every configuration needs, so each test only has to say what
// it is actually about.
func base(t *testing.T) {
	t.Helper()
	t.Setenv("MAILROOM_PUBLIC_URL", "https://mail.example.com")
	t.Setenv("MAILROOM_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

// Leftover password configuration must stop the server rather than be ignored. An operator
// whose MAILROOM_PASSWORD_HASH is silently dropped believes they still have a way in.
func TestPasswordConfigurationIsRefusedRatherThanIgnored(t *testing.T) {
	for _, name := range []string{"MAILROOM_PASSWORD_HASH", "MAILROOM_TOTP_SECRET"} {
		t.Run(name, func(t *testing.T) {
			base(t)
			t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")
			t.Setenv("MAILROOM_GOOGLE_CLIENT_ID", "id")
			t.Setenv("MAILROOM_GOOGLE_CLIENT_SECRET", "secret")
			t.Setenv(name, "something")

			_, err := Load()
			if err == nil {
				t.Fatal("expected startup to fail")
			}
			if !strings.Contains(err.Error(), "password login has been removed") {
				t.Fatalf("the error should explain why, got: %v", err)
			}
		})
	}
}

func TestAuthModeLocalIsRefused(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_MODE", "local")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("MAILROOM_AUTH_MODE=local should fail with an explanation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "adopt-owner") {
		t.Fatalf("the error should say how to move an existing account, got: %v", err)
	}
}

func TestAuthProvidersRefusesLocal(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "local,google")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "password login has") {
		t.Fatalf("naming local should fail, got: %v", err)
	}
}

// There is no login method that can be assumed, so starting with none configured would leave
// the operator interface either unreachable or unguarded.
func TestNoLoginMethodIsAStartupError(t *testing.T) {
	base(t)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "no login method is configured") {
		t.Fatalf("want a startup error naming the options, got: %v", err)
	}
	for _, hint := range []string{"google", "MAILROOM_OIDC_", "forward"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("the error should mention %q: %v", hint, err)
		}
	}
}

// Google is the out-of-the-box case: naming it configures an issuer by itself and reuses the
// OAuth client that already links Gmail mailboxes.
func TestGoogleNeedsNothingBeyondTheLinkingClient(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_ID", "linking-id")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_SECRET", "linking-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.OIDC) != 1 {
		t.Fatalf("want one provider, got %d", len(cfg.Auth.OIDC))
	}
	p := cfg.Auth.OIDC[0]
	if p.Issuer != googleIssuer {
		t.Errorf("want the fixed Google issuer, got %q", p.Issuer)
	}
	if p.ClientID != "linking-id" || p.ClientSecret != "linking-secret" {
		t.Errorf("want the linking client reused, got %q/%q", p.ClientID, p.ClientSecret)
	}
	// The login callback is a different path from the linking one, which is the thing an
	// operator has to register at Google.
	if p.CallbackPath != "/auth/google/callback" {
		t.Errorf("unexpected callback path %q", p.CallbackPath)
	}
}

// A provider-specific client wins over the linking one, so an operator who wants sign-in and
// mailbox linking on separate OAuth clients can have that.
func TestGoogleProviderClientOverridesTheLinkingClient(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_ID", "linking-id")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_SECRET", "linking-secret")
	t.Setenv("MAILROOM_OIDC_GOOGLE_CLIENT_ID", "login-id")
	t.Setenv("MAILROOM_OIDC_GOOGLE_CLIENT_SECRET", "login-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auth.OIDC[0].ClientID; got != "login-id" {
		t.Fatalf("want the provider-specific client, got %q", got)
	}
}

// Naming Google with no client anywhere should say which variables are missing, including
// the linking ones, rather than reporting a missing issuer nobody has to set.
func TestGoogleWithoutCredentialsExplainsWhichOnes(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "ISSUER") {
		t.Fatalf("the issuer is known for Google and should not be demanded: %v", err)
	}
	if !strings.Contains(err.Error(), "MAILROOM_GOOGLE_CLIENT_ID") {
		t.Fatalf("the error should name the linking client, got: %v", err)
	}
}

func TestSignupModeMustBeRecognised(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "forward")
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("MAILROOM_SIGNUPS", "opne")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAILROOM_SIGNUPS") {
		t.Fatalf("a misspelled mode should fail, got: %v", err)
	}
}

// A knob that looks configured and does nothing is worse than no knob. MAILROOM_REDIS was
// read into a field no code ever used, and there is no Redis client in the binary at all, so
// an operator who set it believed they had configured something.
func TestInertRedisSettingIsReportedRatherThanIgnored(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "forward")
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("MAILROOM_REDIS", "redis://localhost:6379")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("it must not prevent startup: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("setting it should produce a warning")
	}
	if !strings.Contains(cfg.Warnings[0], "MAILROOM_REDIS") {
		t.Fatalf("the warning should name the variable, got: %q", cfg.Warnings[0])
	}
}

func TestNoWarningsWhenNothingInertIsSet(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "forward")
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "10.0.0.0/8")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("an ordinary configuration should warn about nothing, got %v", cfg.Warnings)
	}
}

// The store accepts sqlite:// and nothing else, so the documentation must not offer postgres
// and the refusal must be legible.
func TestPostgresURLIsRefusedByTheStoreNotHalfSupported(t *testing.T) {
	if strings.Contains(readDoc(t, "../../docs/deploying.md"), "Or `postgres://") {
		t.Fatal("deploying.md offers a postgres URL the store rejects at startup")
	}
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// google sets the one login method every attachment test below needs, so those tests are
// about attachment settings rather than about authentication.
func googleAuth(t *testing.T) {
	t.Helper()
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_ID", "id")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_SECRET", "secret")
}

func TestAttachmentDefaultsSitBesideTheDatabase(t *testing.T) {
	base(t)
	googleAuth(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	// The default database is /data/mailroom.db, so blobs belong on the same volume rather
	// than on a path only one deployment happens to have.
	if cfg.Attachments.Dir != "/data/attachments" {
		t.Errorf("want the blob directory beside the database, got %q", cfg.Attachments.Dir)
	}
	if cfg.Attachments.TTL.String() != "15m0s" {
		t.Errorf("the default retention should be short, got %s", cfg.Attachments.TTL)
	}
}

func TestAttachmentDirectoryFollowsTheDatabase(t *testing.T) {
	base(t)
	googleAuth(t)
	t.Setenv("MAILROOM_DB", "sqlite:///srv/mail/mailroom.db")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Attachments.Dir != "/srv/mail/attachments" {
		t.Errorf("moving the database should move the blobs, got %q", cfg.Attachments.Dir)
	}
}

// The setting is a retention policy on somebody's mail, so an unreadable or unreasonable one
// stops the server rather than quietly becoming something else.
func TestAttachmentRetentionIsBounded(t *testing.T) {
	for _, tc := range []struct{ value, wants string }{
		{"forever", "must be a duration"},
		{"48h", "must be between"},
		{"1s", "must be between"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			base(t)
			googleAuth(t)
			t.Setenv("MAILROOM_ATTACHMENT_TTL", tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("%q should be refused", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("want an error saying %q, got: %v", tc.wants, err)
			}
		})
	}
}

func TestAttachmentSizesAcceptTheUnitsAnOperatorWrites(t *testing.T) {
	base(t)
	googleAuth(t)
	t.Setenv("MAILROOM_ATTACHMENT_QUOTA", "64MiB")
	t.Setenv("MAILROOM_ATTACHMENT_CACHE_MAX", "2GiB")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Attachments.OwnerQuota != 64<<20 {
		t.Errorf("quota parsed as %d", cfg.Attachments.OwnerQuota)
	}
	if cfg.Attachments.InstanceCap != 2<<30 {
		t.Errorf("cache cap parsed as %d", cfg.Attachments.InstanceCap)
	}
}

// A per-user quota above the instance cap can never be the thing that refuses anybody, which
// is worth saying out loud rather than leaving as a setting that looks configured.
func TestAQuotaAboveTheInstanceCapWarns(t *testing.T) {
	base(t)
	googleAuth(t)
	t.Setenv("MAILROOM_ATTACHMENT_QUOTA", "2GiB")
	t.Setenv("MAILROOM_ATTACHMENT_CACHE_MAX", "512MiB")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(strings.Join(cfg.Warnings, " "), "ATTACHMENT_QUOTA") {
		t.Errorf("want a warning about the quota, got %v", cfg.Warnings)
	}
}

// held sets a login as well, because Load validates one and these tests are about retention.
func held(t *testing.T) {
	t.Helper()
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "forward")
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "127.0.0.1/32")
}

// The default is the whole point of this setting. An instance that has to be configured
// before it stops accumulating message bodies is an instance that never does.
func TestHeldActionsExpireWithoutBeingConfiguredTo(t *testing.T) {
	held(t)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HeldTTL != defaultHeldTTL {
		t.Fatalf("MAILROOM_HELD_TTL unset gave %s, want %s", c.HeldTTL, defaultHeldTTL)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("the default should warn about nothing, got %v", c.Warnings)
	}
}

func TestHeldTTLIsReadAndBounded(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"24h", ""},
		{"5m", ""},
		{"720h", ""},
		{"1m", "must be between"},
		{"721h", "must be between"},
		{"soon", "must be a duration"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			held(t)
			t.Setenv("MAILROOM_HELD_TTL", tc.value)

			c, err := Load()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("%q should be accepted, got: %v", tc.value, err)
				}
				if c.HeldTTL.String() != tc.value && c.HeldTTL <= 0 {
					t.Fatalf("%q loaded as %s", tc.value, c.HeldTTL)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%q should be refused with %q, got: %v", tc.value, tc.want, err)
			}
		})
	}
}

// Turning retention off is allowed and is not quiet about it: unset, this table is the one
// place in the database that accumulates whole messages with nothing to bound it.
func TestTurningHeldRetentionOffWarns(t *testing.T) {
	for _, value := range []string{"0", "off", "never"} {
		t.Run(value, func(t *testing.T) {
			held(t)
			t.Setenv("MAILROOM_HELD_TTL", value)

			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.HeldTTL != 0 {
				t.Fatalf("%q gave a TTL of %s, want none", value, c.HeldTTL)
			}
			if len(c.Warnings) == 0 || !strings.Contains(strings.Join(c.Warnings, " "), "waits forever") {
				t.Fatalf("turning retention off should warn, got %v", c.Warnings)
			}
		})
	}
}
