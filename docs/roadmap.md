# Roadmap

Ordered so that the two things which are expensive to retrofit — authorization and the
provider seam — land before the surface area that depends on them.

## P1 — Spine and read path — **built**

Running end to end against a local instance, and since then against a real Google OAuth
client: the provider, the linking flow and the token sealing have all been exercised on a
live mailbox, and the behavioural conformance suite passes there. See
[providers.md](providers.md#status) for what that run does and does not cover.


- Go service, config, SQLite schema for `accounts`, `grants`, `clients`, `audit_log`
- `OperatorAuth` with both kinds of identity connection: an OIDC issuer, or forward-auth
- Embedded UI shell: accounts, grants, audit
- Outbound OAuth for Google; account linking end to end
- MCP OAuth authorization server: dynamic registration, PKCE, consent form
- Grant enforcement gate
- Gmail provider, read-only
- Tools: `mail_search`, `mail_get_message`, `mail_get_thread`, `mail_accounts`

Both the grant gate and the identity seam ship here. Retrofitting authorization is how it
ends up leaky, and a single-mode login is how the abstraction ends up quietly shaped around
one provider.

Done when: two Gmail accounts are linked, a client holds a grant for one of them, and a
search across "all" returns only the granted mailbox.

Verified so far, against a running server with seeded mailboxes: registration, consent with
nothing preselected, approval narrower than the client's request, PKCE, single-use codes,
immediate revocation, grant-filtered tool listing, and scope-limited discovery. That run has
since been repeated against a real Google OAuth client and a live Gmail mailbox, which is
what closes this phase — by hand, though, so it is true as of the last time somebody did it.

## P2 — Write path

- `mail_draft`, `mail_send`, `mail_modify`, `mail_trash`, `mail_get_attachment`
- Batch modify and delete
- Consent form gains write capabilities
- Per-grant send rate limiting
- Untrusted-content marking at the tool boundary

Send is rate-limited in the same commit that introduces it, not later.

Done when: a `draft`-only grant can compose a reply and is refused when it tries to send it.

## P3 — Second provider — **built; Zoho unverified live**

Ahead of the administrative tail on purpose. The provider seam is the thing that must be
proven early — every later feature is cheaper once two implementations exist, and it is the
surface contributors will actually extend.

- `providers/conformance` suite, written as the contract rather than derived from Gmail
- Zoho Mail provider
- Derived threading for providers without native threads
- Exclusive-label semantics exercised properly
- Generic IMAP/SMTP provider, with live conformance against an in-process server

Done when: Zoho passes conformance without a single provider branch in the tool layer.

The tool layer gained no Zoho branches, which was the point. Two differences absorbed at the
provider seam rather than above it: Zoho pages by offset where Gmail pages by token, and a
Zoho message is addressed by folder *and* id where a Gmail message needs only an id — both
hidden inside the opaque native part of a ScopedID.

IMAP followed, and it is the one that made the contract real: the go-imap library ships an
in-memory server, so the behavioural half of the suite runs on every test run with no
credentials at all. Nine behavioural checks, green.

It also found two flaws in the contract itself, which is what a third implementation is for.
`Capabilities()` was required to *equal* the implemented interfaces, which is wrong for a
provider whose sending depends on configuration; it is now a subset check, since overstating
is the failure that matters and understating is sometimes required. And the missing-message
check synthesized a bare id, which is well-formed for Gmail and malformed for IMAP and Zoho
— the suite was quietly encoding one provider's id shape, so the harness now supplies its own.

Zoho was half-built for longer than it should have been: the provider, the OAuth client and
the redirect URI all existed while the route that would have used them did not, so the whole
implementation was unreachable from the interface. The mailboxes page now offers it.

Outstanding: `Live` has never run against Zoho, which needs a real account and an OAuth
client of its own. Gmail's has, by hand and behind `MAILROOM_LIVE_ACCOUNT`.

## P4 — The administrative tail

- `mail_labels`, `mail_filters`, `mail_settings`
- Filters, aliases, vacation responder, forwarding, delegation

The point at which mailroom is genuinely more capable than anything else available.

## P4.5 — Who is allowed to sign up — **built**

Signing in proves who somebody is. It does not decide whether they belong here, and until this
phase nothing did: the first time an identity signed in, a user row was created for it.

That is sufficient exactly when the issuer is the gate. An Authentik or Keycloak instance you
run already decides who exists, so inheriting its answer is right, and a Google Workspace
domain works too through the `hd` claim. It falls apart with personal accounts. Nothing in a
consumer Google token distinguishes one person from anyone else with a Google account, so an
instance pointed at `accounts.google.com` with no required claim accepted **any Google account
in existence**. Ownership isolation held — a stranger saw none of your mail — but they got an
account on your server, consuming your OAuth client's quota and your disk, and the operator
found out by noticing rows they did not create.

So mailroom now has its own answer for the case where the issuer has none:
**`MAILROOM_SIGNUPS`**, defaulting to `closed`.

| Value | Behaviour |
|---|---|
| `closed` | Only identities that already have a user row may sign in. The first ever sign-in claims the instance; everyone after is refused. The safe default. |
| `invite` | The owner issues an invite, redeemed on first sign-in. Suits handing access to named people without running an IdP. |
| `allowlist` | Sign-in permitted for listed addresses or domains, via `MAILROOM_ALLOWED_EMAILS` / `MAILROOM_ALLOWED_DOMAINS`. |
| `open` | Anyone the issuer authenticates gets an account. Correct only when the issuer is already the gate — an internal Authentik group, say. |

Defaulting to `closed` rather than `open` is the point. An instance that quietly accepts
strangers is the kind of mistake discovered late, and the cost of the safe default is one
environment variable for the rare deployment that genuinely wants anyone to join. That default
holds all the way down: an unrecognised mode is a startup error rather than a fallback, the
zero value of the policy is `closed`, and `allowlist` with nothing listed refuses to start
instead of looking configured while admitting nobody. There is no path by which a typo becomes
`open`. Operator-facing configuration is in [deploying.md](deploying.md#signups).

Two behaviours carry most of the weight, and both were settled before anything else. An invite
binds to whoever redeems it rather than to an address named in advance, because an issuer that
permits unverified addresses would otherwise let someone claim an invite meant for another
person — the cost being that an invite reserves nothing for a named person, and whoever holds
the link is who gets in. And a refused sign-in says the instance is not accepting new accounts
rather than that the account is unknown: the distinction between "wrong password" and "no such
user" leaks membership, and this is the same leak wearing different clothes. It extends to the
codes themselves, where unknown, spent, revoked and expired are one refusal, because telling
them apart would help whoever is guessing.

Three more things fell out of building it. Admission is decided inside the transaction that
inserts the user row, so a refused attempt leaves no account behind and two simultaneous
redemptions of one code cannot both succeed. The invite reaches its holder as a link rather
than a code to type into a form, and that link deposits the code in a short-lived cookie
rather than carrying it through the login flow as a query parameter, because a URL ends up in
referrer headers, proxy logs, browser history and the identity provider's view of the request.
And issuing invites needed an owner, which is derived as the earliest user rather than stored
as a role: a role system with one role and no way to assign it would be worse than a rule that
cannot drift out of sync with reality. Earliest is by insertion order rather than
`created_at`, which is stored to the second and would tie for two sign-ins moments apart.

Verified, at the store and through the sign-in middleware: the first sign-in claims a closed
instance and the next stranger is refused; an existing user still signs in after signups are
closed, so closing them locks nobody out; an unset policy refuses rather than admits; the
allowlist matches listed addresses and exact domains, and `example.com` does not admit
`sub.example.com`; a code admits exactly one person and is refused once spent, revoked or
expired; a refused attempt creates no user and ends the session; the invite link stores a code
without revealing whether it is valid; the cookie is cleared after use, so it cannot be spent
on whichever identity signs in next from that browser; and `/invites` is owner-only.

Outstanding: mailroom does not deliver invites — the link is shown once, and getting it to the
person is yours. There is no user list, no way to remove a user and no way to transfer
ownership, so all three are database work. And an allowlist is worth exactly what the issuer's
address verification is worth, which is why it wants an `email_verified=true` requirement
alongside it on any issuer that lets a user assert an address unchecked.

## P5 — Scale-out

- Postgres and Redis adapters for multi-replica deployments
- An S3-compatible blob backend behind the existing `blob.Bytes` interface. The seam is built
  and the local directory is the only implementation; nothing above it — the metadata, the
  signing, the expiry, every authorisation check — knows where the bytes sit. It is the same
  question as Postgres, and it becomes worth answering at the same moment: a second replica
  cannot serve a link the first one's disk is holding.

### Push was dropped, deliberately

An earlier plan had Gmail `users.watch` over Pub/Sub and IMAP IDLE, surfacing new mail as MCP
notifications instead of polling. It is gone, along with the `WatchProvider` interface nothing
implemented and a `sync_cursors` table no code ever read.

MCP is request and response. An agent asks for mail when it wants mail, and mailroom is a thin
proxy to the provider rather than an index that has to be kept fresh — so there was no polling
for push to replace and no cache for it to invalidate. It was scaffolding for a shape the
product does not have.

It becomes worth building the moment the product should be the other way round: mail
*triggering* an agent rather than an agent fetching mail. "Tell me when something from the
landlord arrives" is a real thing to want and a different piece of software, and it costs a
public endpoint for Pub/Sub to push to, a topic with an IAM binding for Gmail's service
account, and a watch renewed per mailbox every seven days — which stops silently when missed.
That is a fair price for a feature somebody wants and a poor one for an interface nobody
implements.

## Before going public

- Provider conformance green for at least two providers
- Threat model reviewed against the implementation, not the design
- No secrets, real addresses, or infrastructure details anywhere in the git history
- `SECURITY.md` reporting path live
- Quickstart verified from scratch on a clean machine by someone who did not write it
- Third-party notices carried by everything that ships, not only by a source clone. Done:
  see [Third-party notices in the binary](#third-party-notices-in-the-binary--built)

## Multi-user — **built**

Every mailbox and grant belongs to a user, and nothing crosses between them. Ownership is an
explicit argument to every store call rather than something read from a context, so an
unscoped query is a compile error rather than a silent read of somebody else's mail; the
enforcement is in the SQL, so a handler that forgets to check still cannot reach across.

Identity is keyed on issuer and subject together, which means adding a second identity
provider creates new users rather than handing one person another's mailboxes.

Verified: cross-user isolation at the store and at the grant gate, including listing,
fetching by id, alias and address lookups, credential reads, unlink, rename, grant revocation
and the audit log — plus an upgrade of a real single-user database, where the first sign-in
adopts the existing mailbox and a second user adopts nothing.

Two bugs surfaced while building it. Ownership indexes were being created in `schema.sql`,
which runs before the migration that adds the columns, so every *existing* install would have
failed to start — fresh ones were fine, which is exactly the shape of bug that reaches
production. And OIDC sign-in had never worked at all: the redirect URI pointed at
`/auth/callback` and no route was ever registered for it, so the only login mode that had
ever been exercised was the local password.

## Password login removed — **built**

The local password is gone: `MAILROOM_AUTH_MODE=local`, `MAILROOM_PASSWORD_HASH`,
`MAILROOM_TOTP_SECRET` and the `hash-password` subcommand with them. Connecting a real
identity provider — an OIDC issuer, or an authenticating proxy in front — is now the only way
to sign in, and configuring neither is a startup error rather than a default.

It went because a password is the weakest link available in a system whose whole job is
holding other people's mail credentials: no revocation beyond editing a variable and
restarting, no device or session policy, no second factor worth the name once the TOTP secret
sits in the same environment file as the hash, and no audit trail beyond a successful login.
What it bought was a first sign-in before any issuer existed, and the previous phase had
already shown what that was worth — the password was the only login mode ever exercised
precisely because it was the only one that needed nothing set up, which is also why an OIDC
bug survived undetected until somebody used it. The full argument is in
[security.md](security.md#operator-login).

Making it cheap to replace was the other half. `google` is recognised without an issuer to
configure and falls back to the `MAILROOM_GOOGLE_CLIENT_*` pair the instance already holds for
linking Gmail, so an existing deployment gains sign-in for one variable and no new credentials.
The catch is that Google matches redirect URIs exactly and the login callback is a different
path from the linking one, so both have to be registered — the one step an upgrade can get
wrong while everything else looks configured.

A leftover password variable is a startup error rather than something ignored, because ignoring
it would leave an operator believing they hold a login that no longer exists. The removal also
deletes a class of code that existed only to bootstrap: bcrypt handling, TOTP enrolment and
verification, and a subcommand whose sole purpose was generating a hash.

The upgrade needed one thing more. An existing operator's mailboxes and grants belong to a
`local` identity, and identity is keyed on issuer and subject, so signing in through an issuer
would otherwise produce a new and empty user — with the old account unreachable and no login
left that could reach it. So `mailroom invite --adopt-owner` mints an invite that moves an
existing account onto whichever login redeems it. The row keeps its id, so mailboxes, grants
and audit history follow untouched and its position as the earliest user is unchanged. It
belongs on the command line rather than behind the UI because the person who needs it cannot
sign in, which makes a shell on the host the only authorisation available — and that is
already enough to read the database it protects.

Outstanding: adoption is aimed at the owner, so an instance where several *other* password-era
identities own mailboxes still needs one invite redeemed per account, issued from the host.

## Attachments out of the conversation — **built**

Attachment bytes no longer travel through MCP in either direction. `mail_get_attachment`
stages a copy on disk and answers with a short-lived signed URL; `mail_upload_url` mints a
single-use signed URL a client PUTs a file to, and `mail_draft` and `mail_send` reference the
resulting handle. A 5 MB PDF used to be about 6.7 MB of base64 in a model's context, where it
could not be read in any case; and the transport's 4 MiB request ceiling meant that in the
other direction a file worth sending did not fit at all.

The upload half is what makes an agent's own file possible: MCP gives this server no access to
the client's filesystem, so the only way those bytes cross is the client making its own HTTP
request, which is exactly what a presigned PUT is for.

Two decisions carry the design. **Revocation beats the signature** — every request under
`/attachments/` re-reads the grant that minted the link, so revoking or narrowing a grant kills
its outstanding links at once. A signed URL that ran to its own expiry would be the one place
in this server where pressing Revoke did not stop a client reading the mail, and it is a bearer
credential sitting in a transcript. And **a caller's content type is never reflected back** —
an uploaded file served from this origin would otherwise be script running against the origin
that holds the operator's session, and the app's CSP does not cover a raw response.

Deployments behind an authenticating proxy need `/attachments/` bypassed for GET and PUT, the
same way `/mcp` already is. Until that lands the feature is inert: the URLs answer with a login
page.

Not solved: a client that cannot make its own HTTP request. Everything here assumes the caller
can PUT and GET, which every MCP host can, but the tool result is the only place that says so.

## Third-party notices in the binary — **built**

MIT, BSD-2-Clause, BSD-3-Clause and Apache-2.0 all ask for the copyright notice and the
permission text to accompany copies of the software, and none of them makes an exception for a
copy in compiled form. Go embeds neither into a binary. So the published image carried
forty-five dependencies and no acknowledgement of any of them, while `docs/deploying.md` and
the README told operators to pull it.

A source clone was never the gap. A clone has `go.mod`, and `go mod download` fetches every
licence alongside the code it belongs to. What ships to an operator is one static binary in a
distroless image, and that is where the notices were missing.

`internal/notices/NOTICES.md` now carries them, and the binary embeds it. Three decisions are
worth keeping.

**The list is generated from the linked set, not from `go.mod`.** `go list -deps
./cmd/mailroom` reports 45 third-party modules; the module graph has 99. The other 54 are test
dependencies of dependencies, build-time tooling — `modernc.org/ccgo` and friends, which
generate the SQLite sources and are not in them — and API surface that resolves but never
links, and none of it is redistributed. Listing all 99 would assert obligations mailroom does
not have and dilute the ones it does. Going the other way, "indirect" is a fact about `go.mod`
and not about the binary: a module reached only through another dependency is linked in
exactly as much as one imported by name, so indirect dependencies are included in full.

**A hand-written list would have been worse than none**, because it goes stale precisely when
a dependency changes, which is the one moment the obligation moves, and nothing about the
staleness is visible: the build stays green and the image still ships. So `make notices-check`
regenerates and compares in CI, the way `make css-check` does for the stylesheet, and a
dependency added without `make notices` turns the build red. `internal/notices` also carries a
weaker version of the same assertion as a Go test — every linked module is named, and nothing
is named that is not linked — which needs no toolchain beyond Go and so runs on any clone.

**The binary can print its own notices.** `mailroom notices` costs an `embed` of about 240 KB
and is the one route that works however the image is run: the runtime base is distroless, so
there is no shell in there to read a file with, and `docker run … notices` and `kubectl exec
… -- /mailroom notices` both need none. The file is also at `/NOTICES.md` in the image, for
`docker cp`, which in exchange does not need a container that starts.

The generator lives in `scripts/notices`, a module of its own, so that the licence detector it
uses is pinned by version and by checksum without appearing in the root `go.mod`. That is the
same bargain the Tailwind binary strikes for the stylesheet: a tool the build can verify, kept
inside the checkout, and nothing extra for somebody who only wants to `go build`.

Two things the detector gives for free are worth naming, because they are the checks nobody
would otherwise write. It stops the build rather than reporting when a module's licence is not
on the allowlist, so a copyleft dependency cannot arrive quietly; and it stops when it cannot
find a licence at all, rather than emitting an entry with an empty text. The allowlist is
written out in the generator rather than inherited from the detector's own embedded copy, so
that widening it is a diff somebody reviews. Apache-2.0 section 4(d) is handled separately
from the licence text, because it is a separate requirement: a module that ships a `NOTICE`
file has the attribution notices in it carried into anything that redistributes the module.
Two do — `github.com/coreos/go-oidc/v3` and `google.golang.org/grpc` — and the generator looks
for one beside every licence rather than listing those two, so a third arriving with a new
dependency is picked up.

The generated list was then checked against the modules themselves rather than taken on trust,
which is where the interesting part was. Three findings, all now fixed.

The classifier had **three modules wrong**. `modernc.org/libc`, `/mathutil` and `/memory` were
read as BSD-2-Clause and are BSD-3-Clause: all three carry a non-endorsement clause worded
"Neither the names of the authors nor the names of the contributors", which scores below the
classifier's threshold against the canonical "Neither the name of the copyright holder".
There are now no BSD-2-Clause dependencies at all.

**Five modules keep two licences in one file.** The four `go.opentelemetry.io` modules have
the Apache-2.0 text followed by a complete BSD-3-Clause block for the parts derived from the
Go standard library, and `github.com/modelcontextprotocol/go-sdk` has Apache-2.0 followed by
a complete MIT text, because contributors who did not consent to its relicensing stayed under
MIT. The full texts were always reproduced, so nothing was missing — but the summary called
each of them Apache-2.0, and the summary is the part a downstream compliance scan reads.

And **two packages that ship carried a licence nobody was reproducing**. `go-jose/v4/json` is
a fork of the standard library's JSON package with its own BSD-3-Clause file under `json/`,
and `google.golang.org/api` vendors a URI template implementation under
`internal/third_party/uritemplates` whose copyright holder — Joshua Tacoma — appeared nowhere.
`modernc.org/libc` is a third: it is a translation of musl, and musl's MIT terms sit in a
`LICENSE-3RD-PARTY.md` beside libc's own BSD. So the generator now also reproduces licence
files at a module root beyond the primary one, and licence files in subdirectories — but only
where a package the binary actually links sits at or below that directory. That last condition
is what keeps the answer about the binary rather than about the module zip:
`modernc.org/mathutil/mersenne` and `segmentio/encoding/json/fuzz` both have real licences
over code nothing here compiles, and neither is listed.

The corrections a person made live in `identified` in the generator, as an override of the
*identification* only — the text still comes from the module in the cache, so an entry that
goes stale can mislabel a correct text but cannot suppress one.

One thing found and deliberately left: `modernc.org/libc` ships a GPL-2.0 `COPYING` under
`testdata/`, over C fixtures with no Go file anywhere beneath them. The toolchain never
compiles it and it is not in the binary. It would matter only if mailroom ever vendored
libc's source tree, and it is recorded here so a later audit does not have to re-establish it.

Outstanding: identification is still autodetection plus the handful of corrections above,
rather than a person having read forty-five files end to end. The notices are also not
surfaced anywhere in the web UI, which is a fair gap — nobody interacting with mailroom over
the network is being given a copy of the binary, so nothing asks for it there.
