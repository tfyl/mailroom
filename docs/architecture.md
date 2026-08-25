# Architecture

## The one idea

There are two entirely separate OAuth relationships in this system, and most existing mail
MCP servers blur them into one. Keeping them apart is what makes everything else possible.

**Outbound** — mailroom holds long-lived provider credentials for each linked mailbox: a Google
refresh token, a Zoho refresh token, an IMAP app password. This is what the mailbox *can* do.

**Inbound** — an MCP client asks mailroom for access. This is what a given client is *allowed*
to do, and it is always a subset of the outbound capability.

Blur them and every client inherits the full power of every credential you hold, which is
what every server surveyed does. Keep them apart and a grant can hand one agent read access
to a single mailbox while mailroom itself holds send authority on four.

## Layers

```
MCP client
    │
    ├─ /register /authorize /token   → OAuth 2.1 authorization server
    │                                    issues tokens bound to a grant
    ├─ /attachments/…                → signed-URL blob routes (GET and PUT)
    │                                    no session; the signature is the credential,
    │                                    re-checked against the live grant per request
    └─ /mcp                          → MCP handler (streamable HTTP)
                                          │
                                          ▼
                                   Grant enforcement
                                   loads the grant, checks account + capability
                                          │
                                          ▼
                                   Account aggregator
                                   fans out, merges, reports partial failure
                                          │
                    ┌────────────┬────────┴───┬────────────┐
                    ▼            ▼            ▼            ▼
                  Gmail        Zoho       Microsoft    IMAP/SMTP
                                            Graph
```

The grant gate sits between the MCP handler and the aggregator. No tool can reach a mailbox
the calling grant does not name, because the only path to a provider runs through it.

Protocol handling itself is the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk),
which owns the Streamable HTTP transport, session handling, version negotiation and schema
generation. Tool input schemas are generated from the handler argument types, so what a
client is shown cannot drift from what the handler actually parses. What mailroom keeps is
the part that is its own: turning a bearer token into a grant, and building a server whose
tool set is exactly what that grant permits.

Browser traffic (`/accounts`, `/grants`, `/audit`, and the consent form on `/authorize`)
goes through `OperatorAuth` instead — see [deploying.md](deploying.md#identity).

## Portability

mailroom is meant to drop into infrastructure other people already run. The test for the core:
it must start with `docker run` and one environment variable, on a laptop, with no external
services at all.

| Concern | Default — zero dependencies | Optional |
|---|---|---|
| Storage | SQLite, one file | Postgres, for multi-replica — not implemented |
| Sessions, OAuth codes | In-process TTL map | Redis, for multi-replica — not implemented |
| Operator identity | An OIDC issuer; `google` needs none configured | Any other issuer; forward-auth header |
| Secrets | Environment variables | File-mounted, any secret manager |
| Frontend assets | Embedded in the binary | — |
| TLS and ingress | Any reverse proxy | Documented, not implemented |

Identity is the one row with no zero-dependency option, and that is deliberate. mailroom
holds other people's mailbox credentials, so it delegates the question of who you are to
something built to answer it — see [security.md](security.md#operator-login). The cost is
bounded in practice: every target already has an identity provider, and `google` reuses the
OAuth client the instance needs for Gmail anyway.

Consequence: the core depends on the standard library plus a short, boring list. No private
modules, no framework that assumes a particular deployment.

## Frontend

Server-rendered `html/template`, embedded via `embed.FS`. The whole UI ships inside the
binary, and the only JavaScript in it is one hand-written 148-line file — no `npm install`,
no node in the image, no bundler, no lockfile to audit, one artifact to release.

The styling is Tailwind and [Basecoat](https://basecoatui.com), which is shadcn/ui's look
written as plain CSS classes rather than as React components. It is compiled by a standalone
binary and the result is committed, so the CSS toolchain is a thing a contributor may reach
for and never a thing a build depends on. [ui.md](ui.md) is the design system.

This is a security decision more than an ergonomic one. The most important page in the
product is an OAuth consent screen, and that pins the technology:

- **A SPA** pushes credentials toward JavaScript-readable storage and puts hundreds of npm
  dependencies in front of a page that grants mailbox access. Server-rendered keeps the
  session in an httpOnly cookie.
- **Canvas UI toolkits** (egui and friends) render without HTML form semantics: nothing a
  browser, a screen reader or an assistive tool can interpret, no accessibility tree, no real
  focus handling, and a multi-megabyte WASM bundle in front of a security decision.

Because the UI's script is one file this server serves, the Content-Security-Policy can name
it and refuse everything else — `default-src 'none'`, `script-src 'self'` for that one file,
`style-src 'self'` for the one stylesheet and `img-src data:` for two inline icons — rather
than enumerating what a bundle is allowed to load. `script-src` was absent entirely until the
UI had a script at all, and the directive that replaced its absence is narrow in the way that
matters: no `'unsafe-inline'`, no `'unsafe-eval'`, no origin but this one.

The rule the script itself is held to is that it may only make a page that already works
faster or clearer. Every control it touches is a real form control posting to a real endpoint,
so the consent screen behaves the same way with the file blocked — which is the property the
policy was protecting when there was no script to admit. [ui.md](ui.md) has the rule in full.

If a richer UI is ever wanted, the server already speaks JSON and a SPA can be added without
touching the core.

## Users and ownership

An instance serves several people. Every mailbox and every grant belongs to exactly one
user, and nothing crosses between them: not the mailbox list, not the consent screen, not a
grant, not the audit log.

Identity is keyed on `(issuer, subject)` rather than on subject or email alone. Subjects are
only unique within an issuer, so two issuers may legitimately hand out the same one, and
email addresses get reassigned inside an organisation. Keying on the pair means adding a
second identity provider creates new users rather than quietly handing someone another
person's mail.

Ownership is passed as an explicit argument to every store call, never read from a context.
That is a deliberate cost: it makes each call site wordier, and it makes an unscoped query a
compile error instead of a silent read of somebody else's mailbox. The enforcement lives in
the SQL, so a handler that forgets to check still cannot reach across users.

The grant gate resolves accounts using the grant's own owner. A grant therefore reaches its
owner's mailboxes and no others, even if its stored account list somehow named one it should
not — which is the backstop behind the consent screen only ever offering your own.

Signing in and granting access to mail stay separate. Authentication asks for no mail scope
at all; linking a mailbox is a later, separately consented step. Somebody can sign in with a
Google account and link a mailbox at a different provider entirely, and the login carries no
access to either.

## Domain model

One canonical model that every provider maps into.

```go
type Account struct {
    ID       AccountID
    OwnerID  user.ID       // the user this mailbox belongs to
    Alias    string        // "work" — the ergonomic key in every tool call
    Provider ProviderID
    Address  string
    Status   AccountStatus // linked | needs_reauth | disabled
}

type Message struct {
    ID          ScopedID    // rendered "<account-id>:<provider-id>"
    Account     string      // alias, for display; never parsed
    ThreadID    ScopedID
    From        Address
    To, Cc, Bcc []Address
    Subject     string
    Date        time.Time
    Snippet     string
    Body        Body        // text and html
    Labels      []LabelID
    Flags       Flags       // read, starred, draft
    Attachments []AttachmentRef
}

type Label struct {
    ID        LabelID
    Name      string
    Kind      LabelKind // system | user
    Exclusive bool      // folder semantics: applying it moves the message
}
```

`Label.Exclusive` is the load-bearing detail. Gmail has non-exclusive labels; Zoho has
exclusive folders *and* non-exclusive labels; IMAP has folders only. One flag lets a single
`mail_modify` tool serve all three without lying about any of them.

What `Exclusive` does not say is *where* a label moves the message, and one destination is not
filing at all. `LabelManager.EffectOfApplying` is where a provider says which of its own ids is
the bin, so that applying it can be gated as the trashing it is rather than as the labelling it
looks like. See [tools.md](tools.md#labels-that-destroy-mail).

Message IDs are namespaced with the **immutable account ID**, not the alias, so that a `get`
following a cross-account `search` routes without ambiguity — and so that renaming a mailbox
mid-session does not invalidate every ID a client is currently holding. The alias travels
alongside as a display field. See [tools.md](tools.md#identifiers).

## Aggregation

`mail_search` takes an `account` parameter accepting an alias, an address, or a list of
either; omit it and the search fans out across every account the grant names. It is the only
tool that fans out on the strength of that parameter. Tools that take message ids route by
the account named inside each id, and the administrative tools take exactly one mailbox and
refuse to guess.

Cross-account search runs per-account queries in parallel, merges by date, and returns a
composite cursor encoding each provider's own pagination state.

Partial failure is normal here — one rate-limited mailbox must not fail the whole call.
Results carry a per-account status block so a degraded answer is visibly degraded rather
than silently short. A model that cannot tell "no results" from "that mailbox was
unreachable" will confidently tell the user the wrong thing.

## Storage

Three tables carry the product: `users`, `accounts` and `grants`. Around them, `clients`
(from dynamic registration), `tokens`, `audit_log` and `invites`.

`accounts.owner_id` and `grants.owner_id` are nullable for one reason: a database created
before multi-user support has rows without an owner, and they have to be openable. The first
user to sign in adopts them, once, and the event is logged. A second user adopts nothing.
Every insert since sets the owner.

Ownership indexes are created by the migration step rather than by `schema.sql`. That file is
applied first, against tables that may predate the column, and indexing a column that does
not exist yet fails startup for exactly the installs the migration exists to rescue.

Provider refresh tokens are sealed with AES-256-GCM under `MAILROOM_ENCRYPTION_KEY`, with a
per-account nonce, before they touch the database. The server refuses to start without that
key rather than generating one, because a silently generated key produces installs that lose
every linked account on the next redeploy.

## See also

- [grants.md](grants.md) — the scope model
- [providers.md](providers.md) — the provider contract
- [security.md](security.md) — threat model
