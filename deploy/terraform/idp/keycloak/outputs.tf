output "issuer" {
  description = "Issuer URL for mailroom."
  value       = "${trimsuffix(var.keycloak_url, "/")}/realms/${var.realm}"
}

output "client_secret" {
  value     = keycloak_openid_client.mailroom.client_secret
  sensitive = true
}

output "mailroom_env" {
  description = "Paste into mailroom's environment. Read the secret with `terraform output -raw client_secret`."
  value       = <<-EOT
    MAILROOM_AUTH_PROVIDERS=keycloak
    MAILROOM_OIDC_KEYCLOAK_ISSUER=${trimsuffix(var.keycloak_url, "/")}/realms/${var.realm}
    MAILROOM_OIDC_KEYCLOAK_CLIENT_ID=${var.client_id}
    MAILROOM_OIDC_KEYCLOAK_CLIENT_SECRET=<terraform output -raw client_secret>
    MAILROOM_OIDC_KEYCLOAK_LABEL=${var.client_name}
    ${var.create_group ? "MAILROOM_OIDC_KEYCLOAK_REQUIRED_GROUP=${var.group_name}" : "# no group gate configured"}

    # Keycloak decides who exists, so it is also the membership list. Then:
    MAILROOM_SIGNUPS=open
  EOT
}
