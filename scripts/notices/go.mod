// A module of its own, so that the licence detector below is pinned by version and by
// checksum without appearing anywhere in the root go.mod. `go build ./...` and `go test ./...`
// at the repository root do not descend into a nested module, so nothing here is compiled
// into mailroom or needed by somebody who only wants to build it.
module github.com/tfyl/mailroom/scripts/notices

go 1.26.4

require go.elastic.co/go-licence-detector v0.10.0

require (
	github.com/cyphar/filepath-securejoin v0.4.1 // indirect
	github.com/google/licenseclassifier v0.0.0-20250213175939-b5d1a3369749 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
)
