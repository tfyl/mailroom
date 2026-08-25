# Keycloak as mailroom's operator login.
#
# Creates the client, a group that gates it, and — the part that is easy to miss — the
# protocol mapper that puts group membership into the token. Without that mapper the client
# works, sign-in succeeds, and MAILROOM_OIDC_*_REQUIRED_GROUP matches nothing, so everybody
# is refused with a configuration that looks correct on both sides.
#
# Signing in grants no access to any mailbox. Linking one is a separate step consented to at
# Google or Zoho afterwards.

terraform {
  required_version = ">= 1.5"
  required_providers {
    keycloak = {
      source  = "keycloak/keycloak"
      version = "~> 5.0"
    }
  }
}

provider "keycloak" {
  url           = var.keycloak_url
  client_id     = var.admin_client_id
  username      = var.admin_username
  password      = var.admin_password
  realm         = "master"
  initial_login = false
}

resource "keycloak_openid_client" "mailroom" {
  realm_id  = var.realm
  client_id = var.client_id
  name      = var.client_name

  # Confidential: mailroom is a server holding a secret, not a browser app. A public client
  # would mean anybody could impersonate it against this realm.
  access_type                  = "CONFIDENTIAL"
  standard_flow_enabled        = true
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false
  service_accounts_enabled     = false

  # Exactly the callback, with no wildcard. A trailing * here is the standard way people make
  # redirect matching "just work", and it turns any open redirect in the app into a token
  # theft. mailroom has one callback and it is known.
  valid_redirect_uris = ["${local.public_url}${var.callback_path}"]
  root_url            = local.public_url
  base_url            = local.public_url

  # Where Keycloak sends the browser after a logout initiated by mailroom.
  valid_post_logout_redirect_uris = [local.public_url]
}

# Puts group membership in the token as a flat `groups` claim. full_path = false so the claim
# reads "mailroom-users" rather than "/mailroom-users", which is what mailroom compares
# against — the leading slash is the usual reason a required group never matches.
resource "keycloak_openid_group_membership_protocol_mapper" "groups" {
  count = var.create_group ? 1 : 0

  realm_id   = var.realm
  client_id  = keycloak_openid_client.mailroom.id
  name       = "groups"
  claim_name = "groups"
  full_path  = false

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

resource "keycloak_group" "mailroom_users" {
  count = var.create_group ? 1 : 0

  realm_id = var.realm
  name     = var.group_name
}

locals {
  public_url = trimsuffix(var.public_url, "/")
}
