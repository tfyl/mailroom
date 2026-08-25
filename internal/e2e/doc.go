// Package e2e drives a whole assembled mailroom over HTTP.
//
// Every other test in this repository exercises one layer against stubs: a handler with a
// fake store, a store with no handler, a provider against httptest. That is how two bugs
// shipped in one evening — a search whose syntax no provider parsed and a search that
// returned nothing for every query — while a conformance suite reported a pass, because each
// half was correct about the half it could see.
//
// So the tests here assemble the server the way cmd/mailroom does: a real SQLite file, the
// real router, the real middleware, the real OAuth server and the real MCP transport, behind
// httptest.NewServer. A client registers itself, walks the authorization flow with PKCE, has
// an operator approve it on the consent screen, exchanges the code, and then speaks MCP over
// Streamable HTTP with the bearer token it was issued. The one thing replaced is the far end
// of the mail provider, because the assertion is about what reached it.
//
// There is nothing in this package outside its tests; this file exists so the directory is a
// buildable package rather than a hole in `go build ./...`.
package e2e
