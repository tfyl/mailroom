package config

import (
	"strings"
	"testing"
	"time"
)

// registration sets a login as well, because Load validates one and these tests are about the
// bound on the endpoint that needs none.
func registration(t *testing.T) {
	t.Helper()
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "google")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_ID", "id")
	t.Setenv("MAILROOM_GOOGLE_CLIENT_SECRET", "secret")
}

// The default is the whole point. POST /register needs no credential and writes a row per
// call, so a bound that has to be configured before it bounds anything is one that is not
// there on any of the deployments that most need it.
func TestRegistrationIsBoundedWithoutBeingConfiguredTo(t *testing.T) {
	registration(t)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RegisterCap.Count <= 0 || c.RegisterInstanceCap.Count <= 0 {
		t.Fatalf("both halves should default on, got %+v and %+v", c.RegisterCap, c.RegisterInstanceCap)
	}
	if c.ClientTTL != defaultClientTTL {
		t.Fatalf("MAILROOM_CLIENT_TTL unset gave %s, want %s", c.ClientTTL, defaultClientTTL)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("the defaults should warn about nothing, got %v", c.Warnings)
	}
}

// And the default has to be a number nobody honest reaches, or defaulting it on is not
// defensible. Twenty an hour from one address is an MCP client reconnecting every three
// minutes for an hour.
func TestTheDefaultBoundIsFarPastAnyHonestUse(t *testing.T) {
	registration(t)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RegisterCap.Count < 20 || c.RegisterCap.Window < time.Hour {
		t.Fatalf("the per-address default is tighter than a person debugging an MCP config: %+v", c.RegisterCap)
	}
	if perSecond(c.RegisterInstanceCap) < perSecond(c.RegisterCap) {
		t.Fatalf("the instance default must not be tighter than one address's: %+v vs %+v",
			c.RegisterInstanceCap, c.RegisterCap)
	}
}

func TestRegistrationLimitsAreReadAndCanBeTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		perAddr  string
		instance string
		wantErr  string
		wantWarn string
	}{
		{name: "read", perAddr: "5/minute", instance: "500/hour"},
		{name: "per-address off", perAddr: "off", instance: "50/hour"},
		{name: "instance off", perAddr: "5/hour", instance: "never"},
		{name: "both off", perAddr: "off", instance: "0", wantWarn: "unbounded"},
		{name: "nonsense", perAddr: "lots", wantErr: "MAILROOM_REGISTER_RATE_LIMIT"},
		{name: "zero count", instance: "0/hour", wantErr: "MAILROOM_REGISTER_INSTANCE_LIMIT"},
		{name: "per-address exceeds instance", perAddr: "100/hour", instance: "10/hour",
			wantWarn: "can never be the one that refuses"},
		// Different units, same question. A per-minute allowance that outruns a per-hour
		// ceiling is the same misconfiguration written another way.
		{name: "units are compared not spellings", perAddr: "10/minute", instance: "100/hour",
			wantWarn: "can never be the one that refuses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registration(t)
			if tc.perAddr != "" {
				t.Setenv("MAILROOM_REGISTER_RATE_LIMIT", tc.perAddr)
			}
			if tc.instance != "" {
				t.Setenv("MAILROOM_REGISTER_INSTANCE_LIMIT", tc.instance)
			}

			c, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error naming %s, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(c.Warnings, " ")
			if tc.wantWarn == "" && warnings != "" {
				t.Fatalf("want no warning, got %q", warnings)
			}
			if tc.wantWarn != "" && !strings.Contains(warnings, tc.wantWarn) {
				t.Fatalf("want a warning containing %q, got %q", tc.wantWarn, warnings)
			}
		})
	}
}

func TestClientTTLIsReadAndBounded(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"168h", ""},
		{"24h", ""},
		{"8760h", ""},
		{"1h", "must be between"},
		{"8761h", "must be between"},
		{"soon", "must be a duration"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			registration(t)
			t.Setenv("MAILROOM_CLIENT_TTL", tc.value)

			c, err := Load()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("%q should have loaded: %v", tc.value, err)
			case tc.want == "":
				if c.ClientTTL <= 0 {
					t.Fatalf("%q loaded as %s", tc.value, c.ClientTTL)
				}
			case err == nil || !strings.Contains(err.Error(), tc.want):
				t.Fatalf("%q should have been refused with %q, got %v", tc.value, tc.want, err)
			}
		})
	}
}

func TestClientTTLOffWarns(t *testing.T) {
	registration(t)
	t.Setenv("MAILROOM_CLIENT_TTL", "off")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientTTL != 0 {
		t.Fatalf("off gave a TTL of %s, want none", c.ClientTTL)
	}
	if !strings.Contains(strings.Join(c.Warnings, " "), "kept forever") {
		t.Fatalf("turning the reaper off should say what that costs, got %v", c.Warnings)
	}
}

// The trusted-proxy list used to be read only when forward-auth was configured. It is the
// same list, and the registration bound needs it whichever login method is in use.
func TestTrustedProxiesAreReadWhateverTheLoginMethodIs(t *testing.T) {
	registration(t)
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "127.0.0.1/32, 10.0.0.0/8")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 {
		t.Fatalf("want both entries, got %v", c.TrustedProxies)
	}
	if c.Auth.Forward != nil {
		t.Fatal("this instance signs in with Google; reading the list must not turn forward-auth on")
	}
}

// And forward-auth still reads the same one, so there is one list rather than two.
func TestForwardAuthUsesTheSameTrustedProxyList(t *testing.T) {
	base(t)
	t.Setenv("MAILROOM_AUTH_PROVIDERS", "forward")
	t.Setenv("MAILROOM_TRUSTED_PROXIES", "10.0.0.0/8")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Forward == nil {
		t.Fatal("forward-auth should be configured")
	}
	if len(c.Auth.Forward.TrustedProxies) != 1 || c.Auth.Forward.TrustedProxies[0] != c.TrustedProxies[0] {
		t.Fatalf("forward-auth has %v, the instance has %v", c.Auth.Forward.TrustedProxies, c.TrustedProxies)
	}
}
