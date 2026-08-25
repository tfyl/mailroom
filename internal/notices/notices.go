// Package notices carries the third-party licence notices into the binary.
//
// MIT, BSD and Apache-2.0 all ask for their copyright notice and permission text to
// accompany copies of the software, and they do not distinguish between a copy in source
// form and a copy in a compiled binary. Go embeds neither, so a published image would ship
// forty-five dependencies and none of their notices. Source distribution was never the gap:
// a clone has go.mod, and `go mod download` fetches every licence with the code. What ships
// to an operator is one static binary in an image with no shell in it.
//
// Embedding rather than shipping the file alone is what makes the notices reachable however
// the image is run. The runtime base is distroless, so there is nothing in there that could
// read a file — but `mailroom notices` needs no shell, which means it works over
// `docker run`, over `kubectl exec`, and on a binary somebody built themselves.
package notices

import _ "embed"

// Text is the generated notice file, built by `make notices` and committed.
//
// Committed rather than generated during the build, for the same reason
// internal/web/static/app.css is: `git clone && go build ./...` has to work with nothing but
// Go installed, and go:embed needs the file to exist before compilation starts. A committed
// artefact can drift from what its sources produce, so `make notices-check` rebuilds it in
// CI and compares, and notices_test.go asserts the weaker property without needing the
// generator at all.
//
//go:embed NOTICES.md
var Text string
