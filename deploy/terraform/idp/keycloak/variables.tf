variable "keycloak_url" {
  description = "Base URL of your Keycloak, such as https://sso.example.com"
  type        = string
}

variable "realm" {
  description = "Realm to create the client in. Not `master` unless you have a reason."
  type        = string
}

variable "admin_client_id" {
  description = "Admin client used by Terraform itself. `admin-cli` for a username and password login."
  type        = string
  default     = "admin-cli"
}

variable "admin_username" {
  description = "Keycloak administrator username."
  type        = string
}

variable "admin_password" {
  description = "Keycloak administrator password."
  type        = string
  sensitive   = true
}

variable "public_url" {
  description = "Where mailroom is reachable, exactly as MAILROOM_PUBLIC_URL is set."
  type        = string

  validation {
    condition     = can(regex("^https?://[^/]+/?$", var.public_url))
    error_message = "public_url must be a bare origin such as https://mail.example.com."
  }
}

variable "callback_path" {
  description = <<-EOT
    Where mailroom receives the callback, which depends on how you configure it:

      MAILROOM_AUTH_PROVIDERS=keycloak  ->  /auth/keycloak/callback   (this default)
      MAILROOM_AUTH_MODE=oidc           ->  /auth/callback

    Keycloak matches redirect URIs exactly unless you use a wildcard, and you should not use
    a wildcard.
  EOT
  type        = string
  default     = "/auth/keycloak/callback"

  validation {
    condition     = startswith(var.callback_path, "/")
    error_message = "callback_path is a path, so it starts with a slash."
  }
}

variable "client_id" {
  description = "OAuth client id mailroom will use."
  type        = string
  default     = "mailroom"
}

variable "client_name" {
  description = "Display name in the Keycloak console."
  type        = string
  default     = "mailroom"
}

variable "create_group" {
  description = "Create a group and the protocol mapper that puts it in the token."
  type        = bool
  default     = true
}

variable "group_name" {
  description = "Name of that group, and what MAILROOM_OIDC_<NAME>_REQUIRED_GROUP should be set to."
  type        = string
  default     = "mailroom-users"
}
