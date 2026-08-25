output "tenant_id" {
  description = "Directory the registration was created in."
  value       = data.azuread_client_config.current.tenant_id
}

output "client_id" {
  description = "Application (client) id → MAILROOM_MICROSOFT_CLIENT_ID. Not a secret: it is public by design."
  value       = azuread_application.mailroom.client_id
}

# Only present when create_client_secret is true, and readable with
# `terraform output -raw client_secret`. It is in Terraform state either way, which is the
# reason create_client_secret exists: state that is not stored like a password should not hold
# one, and the portal will create the secret by hand instead.
output "client_secret" {
  description = "Generated client secret → MAILROOM_MICROSOFT_CLIENT_SECRET."
  value       = one(azuread_application_password.mailroom[*].value)
  sensitive   = true
}

output "redirect_uri" {
  description = "The registered redirect URI. mailroom derives the same string from MAILROOM_PUBLIC_URL."
  value       = local.redirect_uri
}

output "next_step" {
  description = "What to do with the two values"
  value       = <<-EOT
    A registration named ${var.display_name} now exists in ${data.azuread_client_config.current.tenant_id},
    with ${local.redirect_uri} registered as its redirect URI.

    Set these on mailroom:

        MAILROOM_MICROSOFT_CLIENT_ID=${azuread_application.mailroom.client_id}
        MAILROOM_MICROSOFT_CLIENT_SECRET=<terraform output -raw client_secret>
    ${var.sign_in_audience == "AzureADMyOrg" ? "    MAILROOM_MICROSOFT_TENANT=${data.azuread_client_config.current.tenant_id}\n" : ""}
    Pipe the secret rather than copying it, so it does not pass through a terminal or a shell
    history:

        terraform output -raw client_secret | <your secret store>

    ${var.sign_in_audience == "AzureADMyOrg" ? "MAILROOM_MICROSOFT_TENANT is required here because this registration is single-tenant:\n    mailroom's default of `common` is refused with AADSTS50194 at consent." : "MAILROOM_MICROSOFT_TENANT can be left unset: it defaults to `common`, which this\n    registration accepts."}

    Then link a mailbox at ${var.public_url}/accounts. The first consent screen carries an
    unverified-publisher notice, which is what an app without publisher verification looks
    like and not a sign anything is wrong.

    A personal (outlook.com, hotmail, live.com) mailbox has the full capability surface as
    far as anything has tested — including message rules and automatic replies, which read
    without error on a live one. Writing either on a consumer mailbox is untested; if Graph
    refuses, mailroom reports unsupported_by_provider naming the operation.

  EOT
}
