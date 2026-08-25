# Authentik as mailroom's operator login.
#
# This creates the application, the OAuth2 provider and a group that gates it. It grants no
# access to any mailbox: signing in establishes who the human is, and linking a mailbox is a
# separate step consented to at Google or Zoho afterwards. Keeping those apart is what lets
# somebody sign in here and hold mailboxes at a provider this instance has no relationship
# with.
#
# Copy this directory, set the variables, apply, and feed the outputs to mailroom.

terraform {
  required_version = ">= 1.5"
  required_providers {
    authentik = {
      source  = "goauthentik/authentik"
      version = "~> 2024.8"
    }
  }
}

provider "authentik" {
  url   = var.authentik_url
  token = var.authentik_token
}

data "authentik_flow" "authorization" {
  slug = "default-provider-authorization-implicit-consent"
}

data "authentik_flow" "invalidation" {
  slug = "default-provider-invalidation-flow"
}

data "authentik_certificate_key_pair" "signing" {
  name = "authentik Self-signed Certificate"
}

# openid, email and profile. profile is what carries the groups claim in a default Authentik,
# which is why mailroom does not request a separate `groups` scope — several issuers reject
# the whole authorization request for an unknown scope rather than ignoring it.
data "authentik_property_mapping_provider_scope" "scopes" {
  managed_list = [
    "goauthentik.io/providers/oauth2/scope-openid",
    "goauthentik.io/providers/oauth2/scope-email",
    "goauthentik.io/providers/oauth2/scope-profile",
  ]
}

resource "authentik_provider_oauth2" "mailroom" {
  name      = var.application_slug
  client_id = var.application_slug

  authorization_flow = data.authentik_flow.authorization.id
  invalidation_flow  = data.authentik_flow.invalidation.id
  signing_key        = data.authentik_certificate_key_pair.signing.id

  # Strict matching, and exactly the path mailroom will call back on. The path depends on how
  # mailroom is configured and getting it wrong produces a sign-in that fails at the issuer
  # with a correct-looking configuration on both sides — see var.callback_path.
  allowed_redirect_uris = [
    {
      matching_mode = "strict"
      url           = "${local.public_url}${var.callback_path}"
    },
  ]

  property_mappings = data.authentik_property_mapping_provider_scope.scopes.ids
}

resource "authentik_application" "mailroom" {
  name              = var.application_name
  slug              = var.application_slug
  protocol_provider = authentik_provider_oauth2.mailroom.id
  meta_launch_url   = local.public_url
  meta_description  = "Mail MCP server: linked mailboxes, scoped per-client grants"
}

# The group that may sign in. Each member gets their own mailboxes and grants; nothing is
# shared between them, so adding somebody here gives them an empty mailroom rather than
# access to yours.
resource "authentik_group" "mailroom_users" {
  count = var.create_group ? 1 : 0
  name  = var.group_name
}

# Without a binding, every Authentik user can reach the application. mailroom would still
# admit nobody new if MAILROOM_SIGNUPS is closed, but relying on that alone puts the whole
# gate in one place.
resource "authentik_policy_binding" "requires_group" {
  count = var.create_group ? 1 : 0

  target = authentik_application.mailroom.uuid
  group  = authentik_group.mailroom_users[0].id
  order  = 0
}

locals {
  public_url = trimsuffix(var.public_url, "/")
}
