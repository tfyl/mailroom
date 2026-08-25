# Deploying

## Minimum

```sh
docker run -p 127.0.0.1:8080:8080 \
  -v ./data:/data \
  -e MAILROOM_ENCRYPTION_KEY="$(openssl rand -base64 32)" \
  -e MAILROOM_PUBLIC_URL="https://mail.example.com" \
  -e MAILROOM_AUTH_PROVIDERS="google" \
  -e MAILROOM_GOOGLE_CLIENT_ID="..." \
  -e MAILROOM_GOOGLE_CLIENT_SECRET="..." \
  ghcr.io/tfyl/mailroom:main
```

The port is published on loopback because `MAILROOM_PUBLIC_URL` is an HTTPS address, which
means something else terminates TLS in front. Publishing on every interface as well would leave
the plaintext port reachable beside it, and `-p` writes its rules ahead of most host firewalls,
so the firewall you assumed was covering it usually is not. Widen it when you mean to.
`deploy/docker-compose.yml` binds the same way.

`:main` moves on every merge. That is fine for trying it and wrong for anything you depend on,
because a server holding live mailbox credentials should not change under you without a
decision — resolve it to a digest and pin that. There is no `:latest`: it is published only by
a `v*` tag and no release has been cut, so naming it fails at pull time.

State is one SQLite file under `/data`, and sessions are in-process. No database, no cache, no
queue.

Signing in is the one thing that needs something outside the box, because there is no built-in
password — see [Identity](#identity). The Google client above is the one you already register to
link a Gmail mailbox, so naming `google` as a login provider adds a variable rather than a
dependency. `scripts/setup.sh` produces all of this, including the Console steps Google requires.

`MAILROOM_PUBLIC_URL` must be the externally reachable URL, because OAuth redirect URIs and
the `.well-known` discovery documents are derived from it. Getting it wrong produces
redirect-mismatch errors at the provider.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `MAILROOM_PUBLIC_URL` | — | Required. External base URL. |
| `MAILROOM_ENCRYPTION_KEY` | — | Required. Base64, 32 bytes. Seals stored provider tokens. |
| `MAILROOM_LISTEN` | `:8080` | Bind address. |
| `MAILROOM_DB` | `sqlite:///data/mailroom.db` | SQLite only. See [Scaling out](#scaling-out). |
| `MAILROOM_AUTH_PROVIDERS` | — | Required. How operators sign in. See [Identity](#identity). |
| `MAILROOM_SIGNUPS` | `closed` | Who may create an account. See [Signups](#signups). |
| `MAILROOM_LOG_LEVEL` | `info` | |
| `MAILROOM_SEND_RATE_LIMIT` | `20/hour` | Per grant. |
| `MAILROOM_REGISTER_RATE_LIMIT` | `20/hour` | Client registrations per client address. `off` to disable. See [Bounding client registration](#bounding-client-registration). |
| `MAILROOM_REGISTER_INSTANCE_LIMIT` | `200/hour` | Client registrations across the whole instance. `off` to disable. |
| `MAILROOM_CLIENT_TTL` | `168h` | How long a client registration that never became a grant is kept. `24h` to `8760h`, or `off`. |
| `MAILROOM_TRUSTED_PROXIES` | — | CIDR list of sources that may speak for somebody else. Required for `forward`; read whatever the login method is. |
| `MAILROOM_HELD_TTL` | `72h` | How long an action queued by a grant in `hold` mode waits before it is discarded. `5m` to `720h`, or `off`. See [Held actions](grants.md#how-long-a-held-action-waits). |
| `MAILROOM_ATTACHMENT_DIR` | beside `MAILROOM_DB` | Where attachment bytes are staged. See [Attachments](#attachments). |
| `MAILROOM_ATTACHMENT_TTL` | `15m` | How long staged bytes live, and how long a link lasts. Max `24h`. |
| `MAILROOM_ATTACHMENT_QUOTA` | `128MiB` | Staged bytes per user. |
| `MAILROOM_ATTACHMENT_CACHE_MAX` | `512MiB` | Staged bytes across the instance. |

Provider OAuth clients are yours — you register them with Google, Zoho and Microsoft, and the
credentials never leave your deployment.

| Variable | Notes |
|---|---|
| `MAILROOM_GOOGLE_CLIENT_ID` / `_SECRET` | From your own Google Cloud project |
| `MAILROOM_ZOHO_CLIENT_ID` / `_SECRET` | From the Zoho API console |
| `MAILROOM_ZOHO_REGION` | Zoho data centre suffix; `com` by default. See [Zoho setup](#zoho-setup) |
| `MAILROOM_MICROSOFT_CLIENT_ID` / `_SECRET` | From your own Azure app registration |
| `MAILROOM_MICROSOFT_TENANT` | Tenant segment in the login URLs; `common` by default. See [Microsoft setup](#microsoft-setup) |

### Google setup

Create an OAuth client of type *Web application* in your own Google Cloud project, enable the
Gmail API, and add `${MAILROOM_PUBLIC_URL}/accounts/link/google/callback` as an authorized
redirect URI.

If you also sign in with Google, that flow has its own callback and needs registering on the
same client — see [Google, out of the box](#google-out-of-the-box).

Request the full scope set on every login. **Re-authenticating replaces scopes rather than
merging them**, so logging in with a short list silently drops the rest, and the failure
arrives much later as `403 insufficient authentication scopes` on an unrelated call.

| Scope | Enables |
|---|---|
| `gmail.modify` | Read, label, modify |
| `gmail.compose` | Drafts |
| `gmail.send` | Send |
| `gmail.settings.basic` | Filters, aliases, vacation responder |

Delegation is missing from that list on purpose. It needs `gmail.settings.sharing`, mailroom
never requests it, and adding a scope makes every already-linked mailbox re-consent — so
`mail_settings delegates` reports `unsupported_by_provider` on every Gmail account, Workspace
or not.

Grant only the scopes matching the capabilities you intend to hand out. A deployment that
will never send should not hold `gmail.send`.

### Zoho setup

Register a *Server-based Application* in the [Zoho API console](https://api-console.zoho.com/)
and add `${MAILROOM_PUBLIC_URL}/accounts/link/zoho/callback` as an authorized redirect URI.
Then link a mailbox from the mailboxes page, the same way as Gmail.

**Register the client in the same data centre as the mailbox.** Zoho partitions accounts by
region, and neither half of the flow crosses one: consent is granted at
`accounts.zoho.<region>` and the mailbox is read at `mail.zoho.<region>`. A client registered
at `api-console.zoho.com` cannot authorize a mailbox in the EU data centre whatever the
credentials say. Set `MAILROOM_ZOHO_REGION` to the suffix your account uses — `com`, `eu`,
`in`, `com.au` or `jp` — and note that it is a single setting for the instance, so one
deployment links mailboxes from one region.

| Scope | Enables |
|---|---|
| `ZohoMail.messages.ALL` | Read, search, send |
| `ZohoMail.folders.ALL` | Folders, and the moves that applying one implies |
| `ZohoMail.tags.ALL` | Labels |
| `ZohoMail.accounts.READ` | The mailbox address, read once while linking |

Linking asks for offline access with the consent screen forced. Both are required rather than
cautious: Zoho issues a refresh token only for an offline grant, and only alongside a consent
somebody actually sees — an authorization that rides a standing consent comes back with an
access token and nothing else, and the mailbox then works for exactly one hour. Zoho keeps at
most twenty refresh tokens per user per client and quietly discards the oldest beyond that, so
an instance that re-links the same mailbox repeatedly will eventually break an earlier link.

Zoho itself has never been run against a live mailbox — see
[providers.md](providers.md#status). The linking flow is written to the published
documentation and tested against a stub, which is not the same as knowing it works.

### Microsoft setup

One app registration serves both Microsoft 365 work or school mailboxes and personal
outlook.com, hotmail.com and live.com ones. In the
[Microsoft Entra admin center](https://entra.microsoft.com), under **Entra ID → App
registrations → New registration** (the Azure portal reaches the same pages, and Microsoft's
own docs still disagree about the breadcrumb — look for *App registrations*):

1. **Supported account types:** the option whose `signInAudience` is
   `AzureADandPersonalMicrosoftAccount`. Microsoft has two label sets live in its own docs
   for the same four choices — the newer drop-down calls it *Any Entra ID Tenant + Personal
   Microsoft accounts*, the older radio list *Accounts in any organizational directory and
   personal Microsoft accounts (…)*. Either way it is the one that matches the default
   `common` tenant below; a single-tenant option means only your own organisation's mailboxes
   can ever be linked.
2. **Redirect URI:** platform **Web**, value `${MAILROOM_PUBLIC_URL}/accounts/link/microsoft/callback`.
   It must be the *Web* platform, not *Single-page application*: mailroom exchanges the code
   from the server with a client secret, and a SPA registration refuses a request carrying one.
3. **Certificates & secrets → New client secret.** The value is shown once and never again.
   It is `MAILROOM_MICROSOFT_CLIENT_SECRET`; the *Application (client) ID* on the overview
   page is `MAILROOM_MICROSOFT_CLIENT_ID`. Microsoft caps a secret at 24 months and recommends
   under 12, so this is a rotation you will do again — and a tenant that enforces credential
   standards may cap it lower or refuse to issue client secrets at all, in which case there is
   no way to configure this connector without changing that policy.
4. **API permissions → Add a permission → Microsoft Graph → Delegated permissions**, and add
   the five in the table below.

| Permission | Enables |
|---|---|
| `offline_access` | A refresh token. Without it the mailbox stops working when the first access token expires |
| `User.Read` | The mailbox address, read once while linking, and the send-as list |
| `Mail.ReadWrite` | Read, search, threads, attachments, drafts, folders, delete, and the categories on a message |
| `Mail.Send` | Send |
| `MailboxSettings.ReadWrite` | Message rules, the automatic-replies setting, and the master category list |

That last row is the one worth checking twice. Message rules are gated on `MailboxSettings.*`
and not on `Mail.ReadWrite`, which grants nothing at all on them — reading rules needs
`MailboxSettings.Read` and writing them needs `MailboxSettings.ReadWrite`. The same is true of
the mailbox's master category list.

All five are delegated permissions a signed-in person can consent to for their own mailbox, so
none of them is marked as requiring a tenant administrator. Read that carefully in the portal:
the *Application* permissions of the same names do require admin consent, and the two columns
sit next to each other.

Two things commonly make an administrator necessary anyway. A tenant with *user consent
restrictions* switched on — which is a common hardening step — needs **Grant admin consent for
&lt;tenant&gt;** on the same page. And a multitenant app registered after November 2020 that asks
for anything beyond basic sign-in runs into risk-based step-up consent, which blocks users in
other tenants from consenting unless the app's publisher is verified. Neither applies to an
instance whose operators all live in the tenant the app is registered in.

**The tenant segment decides which accounts may consent at all.** `MAILROOM_MICROSOFT_TENANT`
becomes the `{tenant}` in `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/authorize`,
and the default `common` is the only value that admits both account kinds: `consumers`
refuses every work or school mailbox and `organizations` refuses every outlook.com one. Set
it to your directory's GUID or domain if this instance should only ever link mailboxes from
your own organisation — that is a narrowing, and it has to match the *supported account types*
chosen at registration.

`offline_access` is what produces a refresh token, and the callback refuses to store a mailbox
that came back without one — a mailbox linked on an access token alone works until that token
expires, which Microsoft varies between sixty and ninety minutes on purpose, and then fails as
a credential error a long way from the linking that caused it.

The consent screen is forced on every link. Unlike Google and Zoho, Microsoft does not need
that to issue a refresh token; it is there so that a re-link cannot quietly ride a consent
granted months ago under a narrower set of permissions, which is exactly the situation
somebody is usually re-linking to get out of.

Refresh tokens rotate. Microsoft issues a new one with every refresh and does not revoke the
old one, but does expect it to be discarded — mailroom stores the replacement as it arrives.
An unused refresh token expires after 90 days, so a mailbox nothing has touched for a quarter
needs re-linking.

IMAP is not the route to a Microsoft 365 mailbox. Basic authentication for IMAP and POP was
removed from Exchange Online in January 2023 and cannot be re-enabled by anyone, including
Microsoft support, and personal outlook.com accounts have required OAuth since September 2024.
(SMTP AUTH is the one protocol still accepting basic authentication; Microsoft's current
timeline disables it by default at the end of December 2026.) So an IMAP link to a Microsoft
mailbox needs an OAuth token anyway — and it would arrive without server-side search, folders,
conversations or message rules, which is most of what makes `mail_filters` and `mail_settings`
meaningful.

**This provider has never been run against a live Microsoft mailbox.** Nobody has registered
an OAuth client for one, so the whole round trip — consent, exchange, Graph — is written to
Microsoft's published documentation and tested against a stub. See
[providers.md](providers.md#status).

## Checking a deployment before somebody else does

`mailroom doctor` reads the configuration, asks the providers what it can, and says plainly
which questions it could not answer. It needs no linked mailbox, so it is worth running the
moment the environment is set rather than after the first failed link.

```console
$ mailroom doctor
ok    Public URL
      https://mail.example.com
ok    Google redirect URI for linking a mailbox
      https://mail.example.com/accounts/link/google/callback — registered
ok    Microsoft client secret
      the client id and secret are a matching pair
?     Microsoft redirect URI
      … Entra answers a registered and an unregistered URI identically before sign-in
--    IMAP/SMTP
      nothing to check — an IMAP mailbox carries its own host and credentials
```

Four markers, and the difference between the last two matters. `ok` and `FAIL` are answers.
`?` means the check ran and the answer could not be established. `--` means there was nothing
to check — a provider with no client configured, or one whose answer this deployment cannot
obtain. Only `FAIL` sets a non-zero exit status.

### What it can check, by provider

| | Google | Microsoft | Zoho | IMAP |
|---|---|---|---|---|
| Redirect URI registered | **yes** | no | no | n/a |
| Client id recognised | **yes** | shape only | no | n/a |
| Client secret valid | no | **yes**, on a single directory | no | n/a |
| Tenant or region sane | n/a | **yes** | name only | n/a |

The gaps are not oversights, and they are the reason this section exists. Google's
authorization endpoint refuses an unregistered redirect URI in a way that can be read without
signing anybody in. **Microsoft and Zoho do not.** Both answer an authorization request naming
an unregistered redirect URI with the same sign-in page as a registered one, because they
compare the URI only after authentication — Zoho does the same for a client id of `garbage`.
A check written from their documentation would report both as registered every time, which is
worse than no check at all, so mailroom reports them as unverified and prints the exact string
to compare by eye.

The Microsoft secret check is the other way round: it is the only one of the three that can be
proven, by asking for a client-credentials token, which succeeds or returns `AADSTS7000215`
and nothing in between. It needs a tenant naming one directory. On `common` — mailroom's
default, and the right setting for linking personal mailboxes — the request is refused for
reasons unrelated to the secret, so the check is skipped rather than misread. Setting
`MAILROOM_MICROSOFT_TENANT` to your tenant id for a single run is a way to test the secret,
if that is the question.

## Linking a mailbox without an OAuth client

Everything above assumes you registered a client with Google. One path needs none: IMAP,
which authenticates with a password rather than a token. For Gmail that password is an *app
password*, and it is the only way to reach a Gmail mailbox with no Google Cloud Console
interaction whatsoever — no OAuth client, no consent screen, no verification. It is therefore
the answer for a deployment nobody is sitting in front of, and the only answer at all for a
mail host that does no OAuth.

Unattended, from a shell on the host:

```sh
MAILROOM_LINK_PASSWORD=... mailroom link-imap \
  -alias personal -address you@example.com \
  -imap imap.gmail.com:993 -smtp smtp.gmail.com:587
```

The password is never a flag. It comes from `MAILROOM_LINK_PASSWORD`, or from standard input
when that is unset — arguments are readable by every process on the machine and are kept in
shell history, which is not where a mailbox password belongs. `-owner` names the user who
will own the mailbox; without it the account that owns the instance does.

Interactively, the same mailbox is attached from the second form on the mailboxes page, which
takes the same fields and behaves the same way. Use whichever suits: the command finishes a
deployment from a terminal, the form suits somebody who has just signed in.

Either way the credentials are checked against the server before anything is stored, so a
typo is an error at link time rather than a mailbox that exists, looks linked, and fails on
first use.

Leave the SMTP server empty to link a mailbox that can only read. The provider withholds the
`send` capability when it has nowhere to send from, so no grant issued against that mailbox
can offer sending at all — which is a reasonable thing to do deliberately.

### Getting an app password

It needs 2-Step Verification switched on: the option does not appear on an account without
it, and it is unavailable on accounts enrolled in Google's Advanced Protection. Generate it
from your Google account's security settings. Google displays it in four groups of four, and
both the command and the form strip whitespace, so paste it either way.

### What the app-password path costs

- **The credential is coarser than an OAuth token.** It is full mailbox access, with no scope
  to narrow and no way to revoke a part of it — revoking the app password is the only lever,
  and it takes the whole mailbox with it. Grants still constrain what any MCP client can do
  with the mailbox; this is about what mailroom itself holds on your behalf.
- **Filters and settings are unavailable, whatever the server.** The IMAP provider implements
  neither `FilterManager` nor `SettingsManager`, so `mail_filters` and every action of
  `mail_settings` answer `unsupported_by_provider` on such a mailbox, however generous the
  grant. Drafts are not implemented on this path either.
- **A Workspace admin can close it two ways**: by turning IMAP off for the organisation, or by
  restricting mail clients to a list of enumerated OAuth client IDs, which an app password is
  not.
- **Google discourages it.** Its own documentation calls app passwords "not recommended and
  unnecessary in most cases". Nothing has been announced about withdrawing them, but the
  direction of travel is clear enough that a long-lived deployment should know how it would
  move to OAuth if it had to.
- **Changing the Google account password revokes every app password on the account**, so a
  routine password change unlinks the mailbox. It comes back with a new app password and a
  re-link.

## Identity

Signing in identifies a person. It never asks for access to mail: linking a mailbox is a
separate step with its own consent, so the account somebody logs in with carries no access to
anything by itself. Somebody can sign in with Google and link a Zoho mailbox.

There are two ways in, and **several can be active at once** — which is what a shared instance
usually wants:

- **An OIDC issuer.** Google, Authentik, Keycloak, Okta, Entra, anything speaking OIDC
  discovery. mailroom runs the login itself.
- **A proxy in front.** Cloudflare Access, oauth2-proxy, Authelia, Tailscale, Pomerium. The
  proxy authenticates and passes identity in a header.

**There is no built-in password login, and no fallback account.** Configuring neither of the
above is a startup error rather than a default — an instance holding other people's mailbox
credentials should not have a way in that nobody chose. If you are upgrading from a password,
see [Removing a password login](#removing-a-password-login); the reasoning is in
[security.md](security.md#operator-login).

### Google, out of the box

Naming `google` is enough:

```sh
MAILROOM_AUTH_PROVIDERS=google
```

Its issuer is known, so none has to be configured, and the login falls back to
`MAILROOM_GOOGLE_CLIENT_ID` / `MAILROOM_GOOGLE_CLIENT_SECRET` — the same OAuth client you
already registered to link Gmail — when the provider-specific `MAILROOM_OIDC_GOOGLE_CLIENT_ID`
and `_SECRET` are unset. An instance that links Gmail therefore gets Google sign-in for one
variable and no new credentials. Set the provider-specific pair to keep sign-in on a separate
OAuth client.

> **Register the login redirect URI, or nothing else here will help you.** Signing in and
> linking a mailbox are two different OAuth flows with two different callbacks, and Google
> matches redirect URIs exactly rather than by prefix:
>
> ```
> ${MAILROOM_PUBLIC_URL}/auth/google/callback           sign-in
> ${MAILROOM_PUBLIC_URL}/accounts/link/google/callback   linking
> ```
>
> Both belong on whichever client serves them — the same one, by default. **An existing
> deployment has only the second**, because until now Google linked mailboxes and did not sign
> anybody in, and nothing about the configuration looks wrong when the first is missing: the
> variables are right, the client is right, the server starts, and the login fails with
> `redirect_uri_mismatch` at the exact moment you have no other way in. It is the one step
> that turns a correct configuration into a locked door. Add it before you switch.

Consumer Google authenticates every Google account in existence, so decide separately who gets
an account here: [Restricting who the issuer authenticates](#restricting-who-the-issuer-authenticates)
for the issuer-side half, [Signups](#signups) for mailroom's own.

### Any other issuer

Name the providers, then configure each with its own prefix. The name becomes the slug in the
callback URL, so it has to stay stable once registered.

```sh
MAILROOM_AUTH_PROVIDERS=google,authentik

MAILROOM_OIDC_AUTHENTIK_ISSUER=https://auth.example.com/application/o/mailroom/
MAILROOM_OIDC_AUTHENTIK_CLIENT_ID=...
MAILROOM_OIDC_AUTHENTIK_CLIENT_SECRET=...
MAILROOM_OIDC_AUTHENTIK_LABEL='Company SSO'
MAILROOM_OIDC_AUTHENTIK_REQUIRED_GROUP=mailroom-users
```

Each provider's redirect URI is `${MAILROOM_PUBLIC_URL}/auth/<name>/callback` — so
`/auth/authentik/callback` above. Register each at its issuer.

Per-provider variables, all optional except the first three:

| Suffix | Meaning |
|---|---|
| `_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET` | Required. `google` supplies all three. |
| `_LABEL` | Button text. Defaults to a sensible name for known issuers. |
| `_REQUIRED_GROUP` | Group claim that must contain this value |
| `_REQUIRED_CLAIM` | `key=value` the id_token must satisfy |
| `_SCOPES` | Override the requested scopes entirely |

> **A group or claim requirement is not a signup policy.** `_REQUIRED_GROUP` and
> `_REQUIRED_CLAIM` decide who your issuer authenticates, which is the whole gate only where
> the issuer is itself the membership list. Pointed at consumer Google with neither set,
> nothing in a personal account's token distinguishes one person from anybody else, so
> everyone who finds the URL authenticates successfully. What decides whether they then get
> an account here is [`MAILROOM_SIGNUPS`](#signups), which refuses everybody after the first
> until you set it.

### One provider, the original variable

`MAILROOM_AUTH_MODE` still selects exactly one provider and is not deprecated — a
single-provider instance is a perfectly good instance. It now accepts `oidc` or `forward`.

```sh
MAILROOM_AUTH_MODE=oidc
MAILROOM_OIDC_ISSUER=https://auth.example.com/application/o/mailroom/
MAILROOM_OIDC_CLIENT_ID=...
MAILROOM_OIDC_CLIENT_SECRET=...
MAILROOM_OIDC_REQUIRED_GROUP=mailroom-users
```

Its callback stays at `${MAILROOM_PUBLIC_URL}/auth/callback` rather than moving under a slug,
so a redirect URI already registered at the issuer keeps working.

### Behind an authenticating proxy

For deployments already fronted by Cloudflare Access, oauth2-proxy, Authelia, Tailscale or
Pomerium. The proxy authenticates and passes identity in a header, which is a real identity
connection rather than a lesser one: the login is somebody else's problem, and it is usually
somebody who does it better.

```sh
MAILROOM_AUTH_PROVIDERS=forward
MAILROOM_FORWARD_HEADER=X-Forwarded-Email
MAILROOM_TRUSTED_PROXIES=10.0.0.0/8
```

| Variable | Notes |
|---|---|
| `MAILROOM_FORWARD_HEADER` | Defaults to `X-Forwarded-Email` |
| `MAILROOM_TRUSTED_PROXIES` | Required here. The same list [client registration](#bounding-client-registration) is bounded by. |

> **The header is only as trustworthy as the network.** Anyone who can reach the port directly
> can forge it. mailroom refuses to start if `MAILROOM_TRUSTED_PROXIES` is empty, and rejects
> the header from any source outside that list. Bind to a private interface as well; do not
> rely on the CIDR check alone. The `docker run` under [Minimum](#minimum) and
> `deploy/docker-compose.yml` both publish on `127.0.0.1` for this reason, which is right
> where the proxy shares the host and wrong the moment it does not.

### The same person, two providers

Identity is keyed on `(issuer, subject)`. Somebody who signs in with Google and later with
Authentik becomes **two users**, each owning their own mailboxes, even with the same email
address on both.

That is deliberate. Merging on email would mean an issuer that lets a user set an unverified
address could hand them somebody else's mail, and email addresses get reassigned inside an
organisation. Merging two identities is a decision with a person's mailbox on the other side
of it, so it belongs to a human rather than a heuristic. There is no merge feature; link the
mailbox again under whichever identity you intend to keep.

### Scopes, and why Google rejects `groups`

mailroom asks for `openid profile email` — the three every OIDC provider implements.

`groups` is not a standard scope. Authentik and Keycloak have one; **Google does not**, and
its authorization endpoint rejects the entire request with `invalid_scope` rather than
ignoring a scope it does not know. Asking for it unconditionally therefore breaks Google
sign-in with an error naming no particular scope.

So it is requested only when something needs it: setting `_REQUIRED_GROUP`, or a
`_REQUIRED_CLAIM` on `groups`, adds it automatically. Configuring the requirement is enough;
you do not also have to know about the scope. Use `_SCOPES` if an issuer wants something
different again.

Login scopes are also entirely separate from mailbox scopes. Signing in with Google asks for
no Gmail access at all, even where the same OAuth client later requests it for linking.

### Restricting who the issuer authenticates

Set a group or claim requirement on every provider, unless everyone with an account at that
issuer should be able to reach the sign-in. With Google and no restriction, that is every
Google account in existence. This is the issuer-side half; [Signups](#signups) is mailroom's
own.

```sh
# Google Workspace: hd is the hosted-domain claim, absent on personal gmail.com accounts.
MAILROOM_OIDC_GOOGLE_REQUIRED_CLAIM=hd=example.com

# Authentik / Keycloak: a group.
MAILROOM_OIDC_AUTHENTIK_REQUIRED_GROUP=mailroom-users

# Keycloak needs a mapper adding groups to the token; without it the claim is absent
# and everybody is refused, which looks exactly like the policy working.
```

A claim requirement compares against the claim's JSON value, so `email_verified=true` works
as written, and a claim holding a list is satisfied when the list contains the value.

### Issuer trailing slashes

Some providers — Authentik among them — return a discovery document whose issuer carries a
trailing slash, and Go's OIDC library compares issuers as exact strings. mailroom normalises
both sides rather than making every operator rediscover this as a confusing startup failure.

### Removing a password login

Earlier versions had a built-in operator password: `MAILROOM_AUTH_MODE=local`, a bcrypt
`MAILROOM_PASSWORD_HASH`, an optional `MAILROOM_TOTP_SECRET`, and a `mailroom hash-password`
subcommand to generate the hash. All four are gone, and connecting an identity provider is now
the only way to sign in.

An instance still configured that way **will not start**. That is on purpose: silently ignoring
a configured password would leave you believing you had a login that no longer exists, and the
error names the variable it found rather than making you guess.

To upgrade:

1. Configure a provider. [Google](#google-out-of-the-box) is the shortest path if you already
   link Gmail — one variable, no new credentials — as long as
   `${MAILROOM_PUBLIC_URL}/auth/google/callback` goes on the OAuth client at the same time.
   Nothing else in the upgrade fails as quietly as leaving that out.
2. Remove `MAILROOM_AUTH_MODE=local`, `MAILROOM_PASSWORD_HASH` and `MAILROOM_TOTP_SECRET`.
   Leaving any of them set is what the startup error above is about.
3. Move your existing account onto the new login, from a shell on the host:

   ```sh
   mailroom invite --adopt-owner
   ```

   It prints a one-time link. Open it, sign in with the new provider, and the account that owns
   this instance answers to that identity from then on. The account keeps its id, so its
   mailboxes, grants and audit history follow without being touched and it remains the owner;
   the old password identity simply ceases to exist. It works whatever
   [`MAILROOM_SIGNUPS`](#signups) says, `closed` included, because it creates no account.

Skip step 3 and you get a *new* user instead, owning nothing, because identity is keyed on
issuer and subject rather than on the address. The exception is a database old enough to
predate multi-user support, where mailboxes have no owner yet and the first sign-in adopts
them — see [Upgrading from a single-user install](#upgrading-from-a-single-user-install).

The adoption invite is a command rather than a page because whoever needs it cannot sign in,
which is the whole problem. What authorises it is a shell on the host and read access to the
database — already more than the invite hands over. Otherwise it behaves like any other
invite: single use, revocable, and expiring in a week unless `--expires` says otherwise.

Losing access to the issuer now costs more than forgetting a password did, and that is the
trade. It is the same one you already make with every other service behind that issuer, the
same command is the way back when an issuer disappears entirely, and where the risk is not
acceptable you can configure two providers — a company issuer and Google, say — so an outage
at one is not an outage at both. Two identity providers is a better answer than a shared
password, because each stays individually revocable and individually audited.

## Signups

Authenticating and belonging here are two different questions. A group or claim requirement
decides who your issuer will let through; `MAILROOM_SIGNUPS` decides who mailroom will create
an account for once they arrive. Where the issuer is not itself the membership list, the second
is the one doing the work.

| Value | Who gets an account |
|---|---|
| `closed` | Nobody new. The default. |
| `invite` | Somebody arriving with an unredeemed invite code. |
| `allowlist` | The addresses and domains you name in the configuration. |
| `open` | Anyone the issuer authenticates. |

| Variable | Default | Notes |
|---|---|---|
| `MAILROOM_SIGNUPS` | `closed` | An unrecognised value is a startup error, never a fallback. |
| `MAILROOM_ALLOWED_EMAILS` | — | `allowlist` only. Comma-separated addresses. |
| `MAILROOM_ALLOWED_DOMAINS` | — | `allowlist` only. Comma-separated, leading `@` optional. Matched exactly, so `example.com` does not admit `sub.example.com`. |

`allowlist` with neither list set refuses to start, because a policy that admits nobody is
`closed` with extra steps and is far more likely to be a variable you forgot to set.

**The first sign-in on a new instance always succeeds**, whatever the policy says — otherwise a
fresh deployment could never be used at all. That account owns the instance, and it is the only
one that may issue invites. Ownership is simply the first user ever created and there is no way
to transfer it, so sign in yourself before telling anybody else the URL.

Closing signups later locks nobody out. The policy is consulted only for an authenticated
identity that has no user row yet; everyone already using the instance carries on as before.

A refused visitor is told that the instance is not accepting new accounts, and is signed out
again so the next request does not loop straight back through a sign-in that cannot succeed.
They are deliberately not told whether the address they used already has an account here.

### Invites

Under `MAILROOM_SIGNUPS=invite` the owner issues codes from `/invites`, each with a note for
your own records and an expiry of a day, a week, a month or never. The resulting link is
displayed once:

```
https://mail.example.com/invite/A1B2C3...
```

`mailroom invite` on the host issues the same thing without the UI, for when the owner cannot
reach it — and `mailroom invite --adopt-owner` issues the different one described under
[Removing a password login](#removing-a-password-login), which moves an existing account to a
new login rather than creating one.

Only a SHA-256 hash of the code is stored, so it cannot be shown again and a copy of the
database hands over no usable invites. Getting the link to the person is yours to arrange;
mailroom sends nothing.

Opening it puts the code in a short-lived cookie and sends the visitor to sign in, rather than
carrying it as a query parameter through the login flow — it is a credential, and a URL ends up
in referrer headers, proxy logs and browser history, with the identity provider seeing it on
the way past. They may then sign in with any method the instance offers.

An invite binds to whoever redeems it rather than to an address named in advance, so it
reserves nothing for a particular person: whoever holds the link is who gets in. It admits
exactly one person, and unredeemed codes can be revoked from the same page. Unknown, spent,
revoked and expired codes are all refused identically, so an issued invite that stops working
looks the same as one that never existed.

### With an external identity provider

Where the issuer is the membership list — an Authentik or Keycloak group, or Google Workspace
through `_REQUIRED_CLAIM=hd=example.com` — `open` is the right setting. Adding a policy on top
of one that already works only means maintaining the same list in two places.

Where the issuer is consumer Google, or anything else anyone can sign up to, `open` is exactly
the mistake it looks like: every account at that issuer becomes an account here. Use `invite`
or `allowlist`, or restrict at the issuer.

`allowlist` compares against the address in the token, so it is worth what the issuer's
verification of that address is worth. On any issuer that lets somebody assert an address it
has not checked, pair it with `_REQUIRED_CLAIM=email_verified=true`.

## Reverse proxies

**Remote MCP clients cannot complete an interactive login at your proxy.** Claude's backend
connects to `/mcp` with no browser attached, so if your proxy demands an interactive session
there, the connection simply fails.

These paths must bypass proxy-level authentication:

```
/mcp
/authorize
/token
/register
/attachments/
/.well-known/oauth-authorization-server
/.well-known/oauth-protected-resource
```

`/attachments/` is a prefix, and it needs **GET and PUT**, not GET alone. It is how a client
fetches an attachment and how it uploads one, and the client doing it is the same one that
cannot log in at your proxy. Until this bypass is in place, `mail_get_attachment` returns a URL
that answers with your proxy's login page and `mail_upload_url` returns one nothing can write
to — the feature looks configured and does nothing.

Bypassing it is safe for the same reason `/mcp` is: the request carries its own credential.
There is no session and no cookie involved — every request under `/attachments/` is refused
unless it presents a signature this server minted, over that exact blob, for that owner, under
a grant that is still live at the moment of the request.

The bearer token mailroom issues is the real gate on the other paths. Everything else — the
accounts, grants and audit pages — stays behind your proxy.

This is the single most common way a self-hosted remote MCP server ends up either broken or
accidentally open. Bypassing too little breaks clients; bypassing everything exposes your
admin UI.

## Bounding client registration

`POST /register` is RFC 7591 dynamic client registration, and it is unauthenticated on purpose:
an MCP client has to be able to introduce itself before any human has seen it, which is why it
is on the bypass list above. Registering grants nothing — every capability still comes from a
consent screen somebody approves — but each call writes a row.

Two limits bound the endpoint and a reclaimer takes back what gets past them. All three are on
by default:

```sh
MAILROOM_REGISTER_RATE_LIMIT=20/hour      # per client address
MAILROOM_REGISTER_INSTANCE_LIMIT=200/hour # across the whole instance
MAILROOM_CLIENT_TTL=168h                  # registrations that never became a grant
```

The per-address limit is the one that keeps an instance usable while one noisy client is
refused; the instance limit is the one a botnet cannot spread its way around. A refusal is
`429` with an OAuth `temporarily_unavailable` error, identical for both, and it names neither.

**What it takes to hit them.** Twenty registrations from one address in an hour is an MCP
client reconnecting every three minutes for a solid hour. Two hundred across the instance is
more first-time clients in one hour than most self-hosted deployments see in a year. Setting
up a new client, or setting one up again after a reinstall, is one registration. If you are
genuinely running into either, raise them — they are counts per window, spelled like
`MAILROOM_SEND_RATE_LIMIT`, and `off` switches one off entirely.

### Which address a caller is counted as

**By `MAILROOM_TRUSTED_PROXIES`, and nothing else.** The address a request is attributed to is
the one that opened the connection, unless that address is on the trusted list — in which case
it is the rightmost address in `X-Forwarded-For` that is not itself on the list. A caller that
is not a configured proxy may send whatever header it likes and is still counted as itself.

That variable used to be read only under `MAILROOM_AUTH_PROVIDERS=forward`. It is now read
whatever the login method is, and it is worth setting for any deployment with something in
front of it:

```sh
# cloudflared or a reverse proxy on the same host
MAILROOM_TRUSTED_PROXIES=127.0.0.1/32
```

Leaving it empty is safe but blunt. Nothing forwarded is believed from anybody, so behind a
proxy **every caller is attributed to the proxy** and the per-address limit becomes a second,
tighter instance-wide one. The startup line reports what is in force — `trusted_proxies`,
`register_limit`, `register_instance_limit` — so you can check which case you are in.

### Reclaiming registrations nobody approved

A rate limit bounds how fast the `clients` table grows, not how large it gets. A registration
that no grant references was never approved by anybody, so `MAILROOM_CLIENT_TTL` deletes those
older than it, hourly. A registration that became a grant is kept for good, including one
whose grant was later revoked or taken off the grants page.

Seven days is far past any live flow: an authorization request expires ten minutes after the
consent screen is rendered, so a client that has not been approved within a week is one that
never will be. A client whose registration is reclaimed and which then tries to authorize is
told its `client_id` is unknown, and registers again. `off` keeps every row forever.

## Attachments

Attachment bytes do not travel through the MCP conversation. `mail_get_attachment` stages a
copy on disk and answers with a signed URL; `mail_upload_url` mints a signed URL a client PUTs
a file to. Both are short-lived, and both need the `/attachments/` bypass above.

The bytes land in `MAILROOM_ATTACHMENT_DIR`, which defaults to an `attachments/` directory
beside the SQLite file — so on the stock deployment it is `/data/attachments`, on the same
volume you already back up, owned by the same uid the container runs as. A bind mount you
chowned to 65532 for the database is already right for this.

**Nothing there is durable, and none of it should be.** Every blob is a copy of something that
already exists — mail in a mailbox, or a file the client still holds — and it is deleted when
`MAILROOM_ATTACHMENT_TTL` passes, by a sweeper that also runs at startup. The default of 15
minutes is deliberately shorter than feels generous: a link exists to be fetched immediately,
and past that the directory stops being a buffer and becomes a second, unindexed copy of the
mailbox sitting next to the database. The ceiling is 24 hours and the server refuses to start
above it.

Two size limits bound what a client can spend: `MAILROOM_ATTACHMENT_QUOTA` per user and
`MAILROOM_ATTACHMENT_CACHE_MAX` across the instance. A pending upload is charged the size it
was promised rather than the nothing it currently holds, so minting upload URLs cannot walk
past a quota that only counts arrived bytes. Reaching either limit refuses the tool call with
the number named; nothing live is evicted to make room, because a link that 404s after being
handed over is a worse failure than a call that says the store is full.

Backing up `MAILROOM_ATTACHMENT_DIR` is not useful and mildly counterproductive — the contents
are minutes old, meaningless without the database rows beside them, and are somebody's mail.

## Upgrading from a single-user install

A database created before multi-user support has mailboxes and grants with no owner. On
startup mailroom adds the ownership columns, and the first user to sign in adopts everything
unowned — once. The adoption is logged:

```
adopted pre-existing mailboxes and grants  user=user_... issuer=https://accounts.google.com
```

A second user adopts nothing, so on a shared instance it matters who signs in first: that is
the account the existing mailboxes end up belonging to. Such a database almost certainly
predates the removal of password login as well, so the same first sign-in is doing both jobs —
[connect a provider](#removing-a-password-login), then sign in yourself before letting anyone
else near it.

Nothing is deleted and no mailbox moves between users afterwards. There is deliberately no way
to reassign a mailbox from the UI — reassignment means handing somebody else's mail to a
different person, which should require a considered database change rather than a button.
Moving a whole account onto a different login is a different question, and that one is
supported: see [Removing a password login](#removing-a-password-login).

## Backups

Two things, and losing either is unrecoverable:

- **`MAILROOM_ENCRYPTION_KEY`** — stored outside the deployment. Without it the database is
  ciphertext and every mailbox must be re-linked.
- **The SQLite file** — accounts, grants, audit history.

`MAILROOM_ATTACHMENT_DIR` is not in that list and does not belong in one: see
[Attachments](#attachments).

Back up the key somewhere that does not depend on the same system as the database. A secret
manager holding both, with no copy elsewhere, is a single point of total loss.

## Scaling out

**One replica, today.** Sessions and authorization codes are held in this process and storage
is SQLite, so a second replica would not see the first one's sessions and both would write the
same file. `MAILROOM_DB` accepts `sqlite://` and nothing else; a `postgres://` URL is refused
at startup rather than half-working.

That is a real limit and not a temporary one to plan around. It is also a small server: a
mailbox gateway for one household or one team is not throughput-bound, and the operational
cost of the single-binary default is most of why it is worth having.

The seams are deliberately drawn for a second implementation — the store interface is narrow
and sessions sit behind their own type — so Postgres and Redis adapters are a contained piece
of work rather than a rewrite. They are on the roadmap and are not built. If you set
`MAILROOM_REDIS`, the server logs a warning saying it does nothing, because a knob that looks
configured and is inert is worse than no knob.

## If you modify mailroom

Every page carries a footer naming the running version and linking to its source. That is how
a deployment answers section 13 of the AGPL, which asks that people who interact with the
program over a network be offered its source — and since a network service is the only way
anybody runs this, it is the only case there is.

The link is a constant in the binary, `sourceRepo` in `internal/web/web.go`, and it points at
`github.com/tfyl/mailroom`. Change anything — patch a provider, edit a template, build from a
fork — and that footer starts offering the wrong source. It sends your users upstream, to code
that is not what they are talking to, and it says nothing to suggest the two differ, so nobody
reading it has any reason to check. Section 13 asks you to offer *your* users the source of
*your* modified version, and a link you did not change does not do that on your behalf. This
is the one obligation in the licence that a running deployment can fail silently.

There is no setting for it. Changing the constant is a source change, which is something
anybody who needs to is already making; and a link an operator can point anywhere is a link
that can be pointed at something that is not the source, while the obligation stays theirs
regardless. So point the constant at your fork in the same change that forks it, or make the
offer somewhere else your users will reliably see it. Either discharges section 13. Doing
neither is the only answer that does not.

Running mailroom unmodified needs nothing. The footer names the revision the binary was built
from and links to that revision rather than to a branch that moves, which is the corresponding
source of exactly what is answering.

## Third-party licences

mailroom links forty-five Go modules into its binary — MIT, BSD-3-Clause and Apache-2.0, and
nothing copyleft. All of those ask for the copyright notice and the permission text to
accompany copies of the software, and they make no exception for a copy in compiled form. Go
embeds neither, so the notices have to be put in deliberately or an image ships forty-five
dependencies and no acknowledgement of any of them.

A few of those modules carry a second licence over part of their own code — a fork of a
standard library package, or a third party translated into Go. Those texts are reproduced too,
which is why the file names copyright holders that no module in the list is named after.

They are in the image twice, because the two ways of reading them fail in different places:

```sh
# Print them. Needs only the ability to run the binary, so it works the same over
# `kubectl exec deploy/mailroom -- /mailroom notices` — the runtime image is distroless
# and has no shell, but this needs none.
docker run --rm ghcr.io/tfyl/mailroom:main notices

# Copy the file out. Needs no working configuration and no container that starts.
docker create --name mailroom-notices ghcr.io/tfyl/mailroom:main
docker cp mailroom-notices:/NOTICES.md .
docker rm mailroom-notices
```

In a clone it is `internal/notices/NOTICES.md`, which is the same file: the binary embeds it.

It is generated rather than written, from what `go list -deps` reports for `./cmd/mailroom` —
the modules that actually link, which is a narrower set than go.mod, because a dependency
needed only to build or to test something is never redistributed and listing it would claim an
obligation that does not exist. `make notices` regenerates it and `make notices-check` runs in
CI, so a dependency added without regenerating turns the build red rather than shipping an
image whose notices describe the previous set.

**If you redistribute a modified mailroom**, this file travels with it, and it is yours to
keep accurate: change a dependency and run `make notices`. Your own obligations under the AGPL
are a separate matter — see [If you modify mailroom](#if-you-modify-mailroom).
