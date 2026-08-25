# Contributing

## Before anything else

This repository is public and its history is permanent. Never commit a secret, a real email
address, an internal hostname, or a token — including in test fixtures, example config, or a
commit message. Deleting a file later does not remove it from history.

Use `example.com` addresses and obviously fake credentials in tests and docs.

## Getting set up

There is no login method mailroom can assume, so the shortest thing that runs is forward-auth,
which trusts a header and needs nothing external:

```sh
export MAILROOM_PUBLIC_URL=http://localhost:8080
export MAILROOM_ENCRYPTION_KEY="$(go run ./cmd/mailroom generate-key)"
export MAILROOM_AUTH_PROVIDERS=forward
export MAILROOM_TRUSTED_PROXIES=127.0.0.1/32
export MAILROOM_DB=sqlite://./mailroom.db

go run ./cmd/mailroom
```

You are then whoever the header says, so reach it with one:

```sh
curl -H 'X-Forwarded-Email: you@example.com' http://localhost:8080/accounts
```

That first request also claims the instance: the first sign-in always succeeds whatever the
signup policy says, and everybody after it is the policy's decision.

To link a mailbox without an OAuth client, see `mailroom link-imap -h`. An IMAP server and a
password involve no provider setup at all, which makes it the quickest way to have real mail
to work against.

No `npm install` and no bundler: the UI is `html/template` embedded with `embed.FS`, so if you
are changing it you are editing Go templates. There is exactly one script,
`internal/web/static/app.js`, and it may only make a page that already works faster or clearer
— every control it touches has to behave the same way with the file absent. Nothing may be
inline, because the policy has no `'unsafe-inline'` and a `<script>` body or an `on*` attribute
would be dead markup that looks live. Read [docs/ui.md](docs/ui.md) before adding to it.

The stylesheet is Tailwind and [Basecoat](https://basecoatui.com), built by a standalone binary
`make css` downloads for you — but the built file is committed, so a clone builds and tests with
Go alone and you only need it if you are changing how the UI looks. Read
[docs/ui.md](docs/ui.md) before touching a template.

## What makes a good pull request

One concern per PR. If the title needs an "and", it is probably two.

New behaviour comes with tests. Provider work must pass `providers/conformance` — that suite
is the contract, and a provider that compiles is not a provider that works.

## Adding a provider

Read [docs/providers.md](docs/providers.md) first. The short version:

1. Implement `Provider`, plus only the capability interfaces you genuinely support. Do not
   stub a method to return "not supported" — leave the interface unimplemented so the
   capability set stays honest.
2. `Capabilities()` must match the interfaces you implemented. Conformance checks this.
3. Pass the conformance suite against a real account.

If your provider does something neither Gmail, Zoho nor IMAP does, say so in the PR before
writing much code. The domain model may need to grow, and that is a design conversation
rather than an implementation detail.

## Security-sensitive areas

Changes to grant enforcement, the OAuth authorization server, credential storage, or
`OperatorAuth` get read closely and will move slower. That is not distrust; it is where the
consequences are.

If you find a vulnerability, do not open a PR fixing it quietly. See [SECURITY.md](SECURITY.md).

## Style

Standard Go. `gofmt`, and `golangci-lint run` before pushing.

Comments explain why, not what. Existing code is the reference.

## License

Contributions are licensed under AGPL-3.0, the same as the project.
