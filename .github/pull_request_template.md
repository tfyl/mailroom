<!-- One concern per PR. If the title needs an "and", it is probably two.

     If this fixes a vulnerability, close this and read SECURITY.md instead. A public pull
     request discloses the bug to everybody running mailroom before they can update. -->

## What this changes, and why

<!-- The why is the half that is hard to recover later. -->

## Checks

- [ ] `go test ./...` and `go vet ./...` pass
- [ ] New behaviour has tests
- [ ] Provider changes pass `providers/conformance` against a real account
- [ ] No real address, hostname or credential anywhere in the diff — `example.com` and
      obvious fakes only, in tests and docs as well as code
