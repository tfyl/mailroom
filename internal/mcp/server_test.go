package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

type fixedGrant struct{ g *grant.Grant }

func (f fixedGrant) GrantForRequest(context.Context, *http.Request) (*grant.Grant, error) {
	if f.g == nil {
		return nil, grant.ErrNotFound
	}
	return f.g, nil
}

// bearer injects the Authorization header the way a real MCP client would.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// connect drives the handler with the SDK's own client, so these tests exercise the real
// protocol rather than a hand-written approximation of it.
func connect(t *testing.T, g *grant.Grant) (*mcp.ClientSession, error) {
	t.Helper()

	srv := NewServer(fixedGrant{g}, NewTools(nil, nil, nil), "https://mail.example.com",
		slog.New(slog.NewTextHandler(discard{}, nil)))

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: ts.URL,
		// The server runs stateless, so GET returns 405 and there is no standalone stream.
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: "token", base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, nil
}

func readGrant(caps ...mail.Capability) *grant.Grant {
	return &grant.Grant{
		ID:       "g1",
		Accounts: []mail.AccountID{"acct_1"},
		Caps:     mail.NewSet(caps...),
	}
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}
	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	slices.Sort(names)
	return names
}

// Every request must be refused once a grant is revoked, including from tools that read the
// grant directly rather than resolving accounts through the gate. mail_accounts is exactly
// that case, and it kept answering for a revoked grant until this check moved to the token
// verifier that every request passes through.
func TestRevokedGrantCannotConnect(t *testing.T) {
	g := readGrant(mail.CapRead)
	revoked := time.Now().Add(-time.Minute)
	g.RevokedAt = &revoked

	if _, err := connect(t, g); err == nil {
		t.Fatal("a revoked grant must not be able to open a session")
	}
}

func TestExpiredGrantCannotConnect(t *testing.T) {
	g := readGrant(mail.CapRead)
	past := time.Now().Add(-time.Hour)
	g.ExpiresAt = &past

	if _, err := connect(t, g); err == nil {
		t.Fatal("an expired grant must not be able to open a session")
	}
}

// A grant naming no mailboxes can do nothing, and must not present as usable.
func TestScopelessGrantCannotConnect(t *testing.T) {
	if _, err := connect(t, &grant.Grant{ID: "g2", Caps: mail.NewSet(mail.CapRead)}); err == nil {
		t.Fatal("a grant naming no mailboxes must not be able to open a session")
	}
}

func TestUnknownTokenCannotConnect(t *testing.T) {
	if _, err := connect(t, nil); err == nil {
		t.Fatal("an unrecognised token must not be able to open a session")
	}
}

// A grant must never be shown a tool it could never call: a visible-but-unusable tool
// invites retries and spends the client's context describing an action it cannot take.
func TestToolsAreFilteredToTheGrant(t *testing.T) {
	cases := []struct {
		name    string
		caps    []mail.Capability
		want    []string
		refused []string
	}{
		{
			name:    "read only",
			caps:    []mail.Capability{mail.CapRead},
			want:    []string{"mail_accounts", "mail_get_message", "mail_get_thread", "mail_labels", "mail_search"},
			refused: []string{"mail_send", "mail_draft", "mail_trash", "mail_modify", "mail_get_attachment", "mail_filters", "mail_settings"},
		},
		{
			name:    "draft without send",
			caps:    []mail.Capability{mail.CapRead, mail.CapDraft},
			want:    []string{"mail_draft"},
			refused: []string{"mail_send"},
		},
		{
			// mail_draft is the second tool two capabilities reach, for the same reason
			// mail_labels is: `draft` writes one and `discard` removes one. A grant holding
			// only `discard` still needs somewhere to call delete from, and the per-action
			// check inside the tool is what keeps the other actions out of reach.
			name:    "discard without draft",
			caps:    []mail.Capability{mail.CapDiscard},
			want:    []string{"mail_draft"},
			refused: []string{"mail_send", "mail_trash", "mail_upload_url"},
		},
		{
			name:    "send",
			caps:    []mail.Capability{mail.CapRead, mail.CapDraft, mail.CapSend},
			want:    []string{"mail_draft", "mail_send"},
			refused: []string{"mail_trash"},
		},
		{
			name:    "attachments",
			caps:    []mail.Capability{mail.CapRead, mail.CapAttachments},
			want:    []string{"mail_get_attachment"},
			refused: []string{"mail_send"},
		},
		{
			name:    "filters",
			caps:    []mail.Capability{mail.CapRead, mail.CapFilters},
			want:    []string{"mail_filters"},
			refused: []string{"mail_settings", "mail_send"},
		},
		{
			name:    "settings",
			caps:    []mail.Capability{mail.CapRead, mail.CapSettings},
			want:    []string{"mail_settings"},
			refused: []string{"mail_filters", "mail_trash"},
		},
		{
			// mail_labels is the one tool two capabilities reach. A grant holding `labels`
			// without `read` was offered no label tool at all, so the capability the consent
			// screen had offered could not be used for anything.
			name:    "labels without read",
			caps:    []mail.Capability{mail.CapLabels},
			want:    []string{"mail_labels", "mail_modify"},
			refused: []string{"mail_search", "mail_get_message", "mail_get_thread"},
		},
		{
			name:    "no capabilities at all",
			caps:    nil,
			want:    []string{"mail_accounts"},
			refused: []string{"mail_search", "mail_send", "mail_draft", "mail_labels", "mail_filters", "mail_settings"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, err := connect(t, readGrant(tc.caps...))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			got := toolNames(t, session)

			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("expected %s to be offered; got %v", want, got)
				}
			}
			for _, refused := range tc.refused {
				if slices.Contains(got, refused) {
					t.Errorf("%s must not be offered to a grant without the capability; got %v", refused, got)
				}
			}
		})
	}
}

// Discovery is not privileged: knowing which mailboxes a grant reaches is a prerequisite for
// using it sensibly, so it is offered even to a grant holding nothing else.
func TestDiscoveryNeedsNoCapability(t *testing.T) {
	session, err := connect(t, readGrant())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !slices.Contains(toolNames(t, session), "mail_accounts") {
		t.Fatal("mail_accounts should be available to every grant")
	}
}

// The SDK generates input schemas from the handler argument types, so a client sees exactly
// what the handler parses. This checks the generation actually happened.
func TestToolsCarryGeneratedInputSchemas(t *testing.T) {
	session, err := connect(t, readGrant(mail.CapRead, mail.CapSend))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
}
