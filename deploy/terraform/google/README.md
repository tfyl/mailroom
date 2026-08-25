# Google Cloud setup

Prepares a Google Cloud project for mailroom: creates it if you want, and enables the Gmail
API. Roughly two minutes.

```sh
terraform init
terraform apply -var project_id=my-mailroom-12345 -var google_account=you@gmail.com
```

`google_account` is the account you expect to own this, and Terraform refuses to act as
anybody else. It is required for a reason worth reading — see
[Which account Terraform is acting as](#which-account-terraform-is-acting-as).

Use an existing project instead:

```sh
terraform apply -var project_id=existing-project -var create_project=false \
  -var google_account=you@gmail.com
```

No billing account is required. The Gmail API has a free quota that covers ordinary use;
billing only matters if you later add Pub/Sub push notifications.

## The scripted path

`scripts/setup.sh` does everything on both sides of the manual step — checks which identity
Terraform will use, applies, prompts for the two values only a human can obtain, generates
the encryption key, and writes a `.env` configured to sign in with Google:

```sh
PROJECT_ID=my-mailroom-12345 PUBLIC_URL=https://mail.example.com ./scripts/setup.sh
```

## Registering the redirect URIs

mailroom needs two redirect URIs on the OAuth client, derived from `MAILROOM_PUBLIC_URL`:

```
<MAILROOM_PUBLIC_URL>/accounts/link/google/callback
<MAILROOM_PUBLIC_URL>/auth/google/callback
```

The first links a Gmail mailbox. The second signs an operator in, and mailroom has no password
login to fall back on, so leaving it out means nobody can reach the instance at all. Google
compares redirect URIs exactly rather than by prefix, and these are different paths — a client
carrying only the linking one looks entirely correct until the first login fails.

Terraform cannot register them. Google publishes no API that can — see below — so this is a
Console step, and the honest thing is to say so rather than to automate a page Google owns.

What Terraform does instead is check. Set `public_url` and `oauth_client_id` and every `plan`
asks Google's authorization endpoint whether each URI is actually registered, so a missed one
is a warning on the next plan rather than a sign-in that fails later for no visible reason.

## Why there is no API

Not "no convenient API" — no API, for anyone who is not Google. Every route was tried
against a live project and each one is closed for a different reason:

| Route | What it returns |
|---|---|
| IAP OAuth Admin API (`google_iap_brand`, `google_iap_client`, `gcloud iap oauth-clients`) | Deprecated January 2025, [shut down 19 March 2026](https://docs.cloud.google.com/iap/docs/deprecations). On an org-less project `iap.googleapis.com/v1/projects/N/brands` answers `400 Project must belong to an organization`. It never had an update method anyway — the REST resource is `name`, `secret`, `displayName`, and `gcloud iap oauth-clients` has create, delete, describe, list and reset-secret but no update. |
| `gcloud iam oauth-clients`, `google_iam_oauth_client` | A different resource that happens to share the name. It belongs to Workforce Identity Federation, needs an organization and a workforce pool, and its `allowedScopes` accepts only `cloud-platform`, `openid`, `email` and `groups` — so it cannot express a Gmail scope and cannot sign in a consumer Google account. |
| Identity Platform / Firebase Auth | Its Google provider requires a `client_id` and `client_secret` you must still create by hand, so it moves the problem rather than removing it. It also publishes no `authorization_endpoint` at all: `https://securetoken.google.com/PROJECT/.well-known/openid-configuration` advertises `response_types_supported: ["id_token"]` and no token endpoint, so a server-side OIDC client discovers it successfully and then builds redirects off an empty URL. It is a relying party, not a provider. Configuring it from Terraform also requires a billing account, which the console path does not. |
| The console's own backend, `clientauthconfig.googleapis.com` | Real, and reachable — `google.identity.clientauthconfig.v1.ClientAuthConfig`, with `ListBrands`, `CreateBrand`, `UpdateBrand`, `DeleteBrand`, `ListClients`, `CreateClient` and `DeleteClient`. There is no `UpdateClient`, so it could not edit a client's redirect URIs even if you could call it. You cannot: every request answers `400 INVALID_ARGUMENT` identically whatever you send, including bodies with deliberately bogus fields, so it is rejecting the caller rather than the arguments. It is a restricted service — Service Usage refuses to enable it with `PERMISSION_DENIED` where `iap.googleapis.com` enables fine — and its OAuth scope cannot be requested at all: the authorization endpoint answers `invalid_scope`, `{invalid=[https://www.googleapis.com/auth/clientauthconfig]}`. |

So the console is the only writer, and this drives the console.

## What breaks it, and how you would notice

The mechanism depends on the shape of a page Google owns and can redesign without notice.
That is the whole of the risk, and it is a real one.

What makes it tolerable is that **the script never reports success on the strength of having
clicked something**. After saving it asks Google's authorization endpoint whether the URI is
actually registered — the same question `mailroom doctor` asks — and fails if the answer is
no. So the failure modes are:

- **Google reorganises the client page.** The script cannot find the field or the button, and
  `terraform apply` fails with the page URL and, if you set `console_trace_dir`, a screenshot,
  the page HTML and the requests the page made. That is what a fix starts from.
- **The saved session expires.** Google redirects to a login page, and the script says so and
  names the capture script. Expect this periodically; it is not a bug.
- **The page changes in a way that silently swallows the edit.** The registration check fails
  after the save, and the apply fails. This is the case that matters, and it is the reason the
  check exists: a script that trusted its own clicks would report success here.

The one thing that would defeat the check is Google changing what its authorization endpoint
returns for an unregistered URI. `scripts/check-redirect-uris.sh` and `mailroom doctor` both
rest on the same behaviour, so that would be visible everywhere at once rather than here alone.

## The step that is still yours

Creating the consent screen and the OAuth client itself. That is the same console and the same
mechanism would extend to it, but this configuration deliberately does not attempt it
unverified. Create the client once at `console.cloud.google.com/auth/clients`, type *Web
application*, with no redirect URIs at all — Terraform adds those — and pass the credentials to
mailroom:

```
MAILROOM_GOOGLE_CLIENT_ID=...
MAILROOM_GOOGLE_CLIENT_SECRET=...
MAILROOM_AUTH_PROVIDERS=google
```

Set `google_sign_in = false` if the instance signs operators in through another issuer; only
the mailbox-linking URI is required then.

## Which account Terraform is acting as

`google_account` is required, and Terraform refuses to create anything if Application Default
Credentials resolve to somebody else. ADC is not the account `gcloud config get-value account`
reports, and on a machine that has ever been pointed at a work account the default ADC *is*
that work account — so an apply meant for a personal project creates it inside the employer's
organization instead. This project was created that way once and had to be deleted.

```
Error: Resource precondition failed

  Terraform is authenticated as you@work.example, but google_account says this project belongs
  to you@gmail.com. Nothing has been created. Fix it by pointing Application Default
  Credentials at the right account — export GOOGLE_APPLICATION_CREDENTIALS at a credentials
  file for it, or run `gcloud auth application-default login` and pick it.
```

The created project's organization is asserted too, because landing inside one is the precise
shape of that failure.

## Two things that will bite you

**While the app is in Testing**, only accounts added as test users can link, and everyone
else gets `access_denied`. Add every mailbox you intend to link, or publish the app.

**A Testing app's refresh tokens expire after seven days.** Mailboxes linked under one will
silently need re-linking a week later. Publish the app before relying on it.

## Scopes

mailroom requests the full set on every link:

| Scope | For |
|---|---|
| `gmail.modify` | Read, label, archive, modify |
| `gmail.compose` | Drafts |
| `gmail.send` | Sending |
| `gmail.settings.basic` | Filters, aliases, vacation responder |
| `userinfo.email` | Reading which mailbox was just linked |

Signing in with the same client asks for none of these. The login flow requests `openid profile
email` only, so the account somebody signs in with carries no mail access by itself.

Google **replaces** granted scopes on re-consent rather than merging them, so a partial
request silently drops the rest and surfaces much later as a `403` on an unrelated call. If
this deployment should never send mail, remove the send scope from `GmailScopes` in
`internal/app/providers.go` rather than hoping a narrower login will stick — and remember
that the grant model already lets you withhold `send` from every client without touching
scopes at all.
