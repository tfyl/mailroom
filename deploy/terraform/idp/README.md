# Connecting an identity provider

mailroom has no password login. Signing in means an OIDC issuer, or an authenticating proxy
in front — see [deploying.md](../../../docs/deploying.md#identity) for the full picture.

These are worked examples for two issuers people actually run. Copy the directory, set the
variables, apply, and `terraform output mailroom_env` prints the environment to paste.

| | |
|---|---|
| [`authentik/`](authentik) | Application, OAuth2 provider, group, and the policy binding that gates it |
| [`keycloak/`](keycloak) | Confidential client, group, and the protocol mapper that puts groups in the token |

Neither is a module you depend on. They are configurations to read and adapt: an issuer you
already run has flows, certificates and naming conventions of its own, and a module that
pretended otherwise would fit nobody.

For Google, there is nothing to apply — see [../google](../google). The client is a Console
step Google exposes no API for, and that directory checks it rather than creating it.

## Three things both examples encode

**The callback path is not the same for everyone.** It is `/auth/<provider>/callback` when you
name providers with `MAILROOM_AUTH_PROVIDERS`, and `/auth/callback` under the older
single-provider `MAILROOM_AUTH_MODE=oidc`, which keeps its original path so that upgrading
does not invalidate a redirect URI already registered. Issuers match exactly. Getting it wrong
produces a sign-in that fails at the issuer while both sides look correctly configured.

**A group requirement needs the claim to actually be in the token.** Authentik carries groups
in the `profile` scope; Keycloak needs a protocol mapper, and by default it emits paths like
`/mailroom-users` rather than `mailroom-users`. Both examples handle this. It is worth knowing
because the failure is that `REQUIRED_GROUP` silently matches nobody — indistinguishable from
a policy that is working.

Note that mailroom does not request a `groups` scope. Several issuers, Google among them,
reject the entire authorization request for an unrecognised scope rather than ignoring it.

**When the issuer is the membership list, say so.** Both examples suggest `MAILROOM_SIGNUPS=open`,
which is right *because* the group binding already decides who exists. mailroom defaults to
`closed` for the case where the issuer decides nothing — consumer Google, where every account
in existence authenticates successfully. Setting `open` without an issuer-side gate is the
mistake that default exists to prevent. See [Signups](../../../docs/deploying.md#signups).

## Another issuer

Anything with a discovery document works. Set `MAILROOM_OIDC_<NAME>_ISSUER`, `_CLIENT_ID` and
`_CLIENT_SECRET`, register `<public-url>/auth/<name>/callback`, and read
[deploying.md](../../../docs/deploying.md#any-other-issuer) for the optional claim and group
requirements.
