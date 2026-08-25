// Package mcp serves the Model Context Protocol endpoint.
//
// Protocol handling comes from the official SDK, which owns the Streamable HTTP transport,
// session handling, version negotiation and schema generation. What lives here is the part
// that is mailroom's own: turning a bearer token into a grant, and building a server whose
// tools are exactly what that grant permits.
//
// Every tool call runs through the grant gate before it can reach a mailbox. There is
// deliberately no other path from this package to a provider.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
)

// Version is stamped at build time.
var Version = "dev"

// GrantResolver turns a request's bearer token into the grant it was issued for.
type GrantResolver interface {
	GrantForRequest(ctx context.Context, r *http.Request) (*grant.Grant, error)
}

type Server struct {
	grants    GrantResolver
	tools     *Tools
	publicURL string
	log       *slog.Logger
}

func NewServer(grants GrantResolver, tools *Tools, publicURL string, log *slog.Logger) *Server {
	return &Server{grants: grants, tools: tools, publicURL: strings.TrimSuffix(publicURL, "/"), log: log}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.Handle("/mcp", s.Handler())
}

// Handler is the MCP endpoint: bearer-token middleware in front of the SDK's Streamable HTTP
// transport.
func (s *Server) Handler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(s.serverFor, &mcp.StreamableHTTPOptions{
		// Stateless: each request is self-contained and carries its own bearer token, so
		// there is no session state worth keeping between them. It also means a client can
		// reconnect or be load-balanced elsewhere without re-establishing anything.
		Stateless: true,

		// The SDK rejects any request that arrives from a loopback address while carrying a
		// non-loopback Host header, which is the right default for an MCP server a browser
		// can reach directly. Behind a reverse proxy it is exactly wrong: nginx, caddy,
		// cloudflared and tailscale serve all forward to 127.0.0.1 while preserving the
		// external Host, so every proxied request looks like a rebinding attempt.
		//
		// Replaced below with a check against the hostname this instance was actually
		// configured for, which is narrower than the rule it turns off: an attacker's
		// hostname still fails, and only the operator's own does not.
		DisableLocalhostProtection: true,
	})

	return s.allowedHosts(auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{

		// Sent in the WWW-Authenticate header on a 401, which is how a client discovers
		// where to authenticate without being told out of band.
		ResourceMetadataURL: s.publicURL + "/.well-known/oauth-protected-resource",
		// A grant may legitimately have no expiry, and its own revocation and expiry checks
		// run in verify below. Without this the middleware would reject every non-expiring
		// grant for want of an `exp`.
		AllowMissingExpiration: true,
	})(streamable))
}

// allowedHosts rejects requests whose Host is neither this instance's configured public
// hostname nor a loopback address.
//
// This is the DNS-rebinding protection, kept but made specific. The attack it defends
// against is a hostname an attacker controls resolving to a loopback address so a victim's
// browser reaches a local server; that hostname will not match the configured one, so it is
// still refused. What no longer gets refused is a reverse proxy forwarding the operator's
// own hostname, which is how this is meant to be deployed.
func (s *Server) allowedHosts(next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if u, err := url.Parse(s.publicURL); err == nil && u.Hostname() != "" {
		allowed[u.Hostname()] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowed[strings.Trim(host, "[]")] {
			http.Error(w, "unrecognised Host header; set MAILROOM_PUBLIC_URL to the hostname "+
				"clients actually reach this server on", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// grantKey carries the resolved grant from the verifier to the tool handlers.
type grantKey struct{}

// verify resolves and validates the bearer token.
//
// The grant is re-read and re-checked on every request rather than trusted from the token,
// so revoking one takes effect immediately instead of at the next token expiry.
func (s *Server) verify(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	g, err := s.grants.GrantForRequest(ctx, withBearer(r, token))
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	if err := g.Valid(time.Now()); err != nil {
		// Revoked, expired, or naming no mailboxes. All three mean the same thing to a
		// client: this token is no longer usable.
		return nil, auth.ErrInvalidToken
	}

	info := &auth.TokenInfo{
		Scopes: g.Caps.Strings(),
		UserID: string(g.ID),
		Extra:  map[string]any{"grant": g},
	}
	if g.ExpiresAt != nil {
		info.Expiration = *g.ExpiresAt
	}
	return info, nil
}

// withBearer makes sure the resolver sees the token the middleware extracted, whichever way
// the client presented it.
func withBearer(r *http.Request, token string) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return clone
}

// serverFor builds an MCP server carrying only the tools this request's grant may call.
//
// Filtering at construction rather than at call time means tools/list never advertises
// something that would always be refused: a visible-but-unusable tool invites retries and
// spends the client's context describing an action it cannot take.
func (s *Server) serverFor(r *http.Request) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mailroom", Version: Version}, nil)

	g := grantFrom(r.Context())
	if g == nil {
		// Unreachable behind the bearer middleware, which rejects before this runs. An empty
		// server is the safe answer if that ever changes.
		return srv
	}
	s.tools.Register(srv, g)
	return srv
}

// grantFrom retrieves the grant the verifier attached to this request.
func grantFrom(ctx context.Context) *grant.Grant {
	if g, ok := ctx.Value(grantKey{}).(*grant.Grant); ok {
		return g
	}
	info := auth.TokenInfoFromContext(ctx)
	if info == nil {
		return nil
	}
	g, _ := info.Extra["grant"].(*grant.Grant)
	return g
}

// requireGrant is the accessor tool handlers use. A missing grant is a programming error
// rather than a client one, so it fails loudly instead of degrading to an empty scope.
func requireGrant(ctx context.Context) (*grant.Grant, error) {
	g := grantFrom(ctx)
	if g == nil {
		return nil, errors.New("no grant on this request")
	}
	return g, nil
}
