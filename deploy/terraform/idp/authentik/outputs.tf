output "issuer" {
  description = "Issuer URL for mailroom. Authentik serves discovery beneath it."
  value       = "${trimsuffix(var.authentik_url, "/")}/application/o/${var.application_slug}/"
}

output "client_id" {
  value = authentik_provider_oauth2.mailroom.client_id
}

output "client_secret" {
  value     = authentik_provider_oauth2.mailroom.client_secret
  sensitive = true
}

# Printed rather than described, because the variable names encode the provider slug and
# working them out by hand is where people go wrong.
output "mailroom_env" {
  description = "Paste into mailroom's environment. The secret is redacted; read it with `terraform output -raw client_secret`."
  value       = <<-EOT
    MAILROOM_AUTH_PROVIDERS=authentik
    MAILROOM_OIDC_AUTHENTIK_ISSUER=${trimsuffix(var.authentik_url, "/")}/application/o/${var.application_slug}/
    MAILROOM_OIDC_AUTHENTIK_CLIENT_ID=${authentik_provider_oauth2.mailroom.client_id}
    MAILROOM_OIDC_AUTHENTIK_CLIENT_SECRET=<terraform output -raw client_secret>
    MAILROOM_OIDC_AUTHENTIK_LABEL=${var.application_name}
    ${var.create_group ? "MAILROOM_OIDC_AUTHENTIK_REQUIRED_GROUP=${var.group_name}" : "# no group gate configured"}

    # Authentik decides who exists, so it is also the membership list. Then:
    MAILROOM_SIGNUPS=open
  EOT
}
