# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub's private vulnerability reporting](https://github.com/tfyl/mailroom/security/advisories/new)
rather than opening a public issue.

Include what you did, what happened, and what you expected. A proof of concept helps but is
not required to report something.

Expect an acknowledgement within a few days. This is a small project maintained in spare
time; that is the honest commitment rather than an SLA that will not be met.

## Scope

In scope:

- Bypassing grant enforcement — reaching an account or capability a grant does not name
- Extracting stored provider credentials
- Forging operator identity, whether against an OIDC issuer or a forward-auth header
- Causing a grant to resolve to an account it does not name, including through alias reuse
- Anything letting an MCP client escalate beyond its issued scope
- Reaching another user's mailboxes, grants, invites or audit rows on a shared instance
- Being admitted as a new user against the configured signup policy, or redeeming an invite
  that is spent, revoked, expired or meant for somebody else

Out of scope:

- Prompt injection causing a model to misuse capabilities its grant legitimately holds. This
  is a known and documented property of the design; see [docs/security.md](docs/security.md).
  A report showing injection escalating *beyond* the granted scope is very much in scope.
- Attacks requiring a compromised host or an already-compromised identity provider
- Missing hardening with no demonstrated impact

## Supported versions

Pre-1.0: only the latest release.
