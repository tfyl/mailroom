terraform {
  required_version = ">= 1.5"
  required_providers {
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 3.0"
    }
  }
}

# --- Which directory is this being created in? ---
#
# Required rather than inferred, and this is the one setting worth being stubborn about. The
# provider falls back to the signed-in CLI account's default tenant, and for a personal
# Microsoft account that default is 9188040d-6c67-4c5b-b112-36a304b66dad — Microsoft's shared
# consumer directory, which every personal account belongs to and nobody administers. It
# cannot hold an app registration at all, so the fallback is never the answer you wanted.
#
# The failure is quiet in the useful sense: `az login` succeeds, reports no error, and leaves
# a session that cannot create anything here.
provider "azuread" {
  tenant_id = var.tenant_id
}

data "azuread_client_config" "current" {}

data "azuread_application_published_app_ids" "well_known" {}

# Microsoft Graph's service principal in this directory. It is read for one reason: the
# permission ids below are resolved from it by name.
data "azuread_service_principal" "graph" {
  client_id = data.azuread_application_published_app_ids.well_known.result["MicrosoftGraph"]
}

locals {
  # mailroom derives this callback from MAILROOM_PUBLIC_URL, and Entra matches redirect URIs
  # character for character, so this is the exact string that has to be registered.
  redirect_uri = "${var.public_url}/accounts/link/microsoft/callback"

  # The delegated Graph permissions mailroom asks for at link time, by name rather than by id.
  # Their ids are resolved from the service principal above, so a name that does not exist
  # fails at plan with an unknown map key — where a mistyped GUID would instead register a
  # permission that is real, wrong, and only noticed when a mailbox will not do something.
  #
  # Keep this in step with `Scopes` in internal/provider/microsoft/microsoft.go. That list is
  # what mailroom asks for at consent; this one is what the directory will allow. Anything
  # mailroom asks for and is not registered here fails at consent, for the operator, with an
  # error that does not name the missing scope.
  graph_scopes = [
    # Without it the token endpoint returns an access token and no refresh token, and the
    # mailbox works until that token expires — which Microsoft deliberately varies between
    # sixty and ninety minutes, so it does not even fail at a predictable time.
    "offline_access",

    # Read once, at link time, for the mailbox's own address.
    "User.Read",

    # Reading, drafting, moving, deleting, and the categories on a message. Mail.Read would be
    # redundant beside it.
    "Mail.ReadWrite",

    # Separate because Graph separates it, which happens to line up exactly with mailroom
    # splitting `send` from `draft`.
    "Mail.Send",

    # The one worth stating, because guessing it wrong is easy: message rules, the
    # automatic-replies setting and the master category list are all gated on this rather than
    # on Mail.ReadWrite, which grants nothing on any of the three.
    "MailboxSettings.ReadWrite",
  ]
}

resource "azuread_application" "mailroom" {
  display_name = var.display_name
  owners       = [data.azuread_client_config.current.object_id]

  # Who is allowed to consent. The default accepts both a personal Microsoft account and a
  # work or school one, which is what MAILROOM_MICROSOFT_TENANT=common (mailroom's own
  # default) authorizes against.
  #
  # These two have to agree. A single-tenant registration refuses `common` outright with
  # AADSTS50194 — at consent, in front of whoever is trying to link a mailbox, not here. See
  # the note on MAILROOM_MICROSOFT_TENANT in outputs.tf.
  sign_in_audience = var.sign_in_audience

  # Entra refuses the personal-account audiences with anything but version 2, and the default
  # is 1. It is also what mailroom wants regardless of audience: it authorizes and redeems
  # against the v2.0 endpoints, which issue v2 tokens.
  api {
    requested_access_token_version = 2
  }

  # `Web`, not SPA and not public client: mailroom is a confidential client and presents a
  # secret at the token endpoint.
  web {
    redirect_uris = [local.redirect_uri]

    # Nothing here uses the implicit flow. mailroom is an authorization-code client with PKCE.
    implicit_grant {
      access_token_issuance_enabled = false
      id_token_issuance_enabled     = false
    }
  }

  required_resource_access {
    resource_app_id = data.azuread_application_published_app_ids.well_known.result["MicrosoftGraph"]

    dynamic "resource_access" {
      for_each = local.graph_scopes
      content {
        id   = data.azuread_service_principal.graph.oauth2_permission_scope_ids[resource_access.value]
        type = "Scope"
      }
    }
  }
}

# Optional, because an operator holding their secrets somewhere with its own rotation may
# prefer to create this by hand and never let it near Terraform state — see the note in
# outputs.tf.
#
# Rotating it is `-replace` on this resource, and it is not seamless: the old secret is
# destroyed as the new one is created, so mailroom holds a dead client secret from that moment
# until the new one reaches its environment and it restarts. Mailboxes already linked survive
# the gap — their refresh tokens are sealed in mailroom's own database and are not re-issued
# by this client — but no new mailbox can be linked during it.
resource "azuread_application_password" "mailroom" {
  count = var.create_client_secret ? 1 : 0

  application_id = azuread_application.mailroom.id
  display_name   = "mailroom (terraform)"

  # Omitted unless asked for, which takes Entra's own default of two years. A directory whose
  # policy caps app secrets shorter than the value set here refuses it at apply rather than
  # silently shortening it.
  end_date = var.secret_end_date != "" ? var.secret_end_date : null
}
