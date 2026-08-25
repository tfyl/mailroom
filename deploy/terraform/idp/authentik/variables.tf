variable "authentik_url" {
  description = "Base URL of your Authentik, such as https://auth.example.com"
  type        = string
}

variable "authentik_token" {
  description = "An Authentik API token with permission to create applications and providers."
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

      MAILROOM_AUTH_PROVIDERS=authentik  ->  /auth/authentik/callback   (this default)
      MAILROOM_AUTH_MODE=oidc            ->  /auth/callback

    The second is the older single-provider form and keeps its original path so that
    upgrading does not invalidate a redirect URI already registered here.

    Issuers match redirect URIs exactly. Setting this wrong produces a sign-in that fails at
    Authentik while both sides look correctly configured, which is a genuinely annoying
    afternoon.
  EOT
  type        = string
  default     = "/auth/authentik/callback"

  validation {
    condition     = startswith(var.callback_path, "/")
    error_message = "callback_path is a path, so it starts with a slash."
  }
}

variable "application_name" {
  description = "Display name in Authentik's application list."
  type        = string
  default     = "mailroom"
}

variable "application_slug" {
  description = "Slug, and the OAuth client id mailroom will use."
  type        = string
  default     = "mailroom"
}

variable "create_group" {
  description = "Create a group and bind it to the application, so only its members may sign in."
  type        = bool
  default     = true
}

variable "group_name" {
  description = "Name of that group. It is also what MAILROOM_OIDC_<NAME>_REQUIRED_GROUP should be set to."
  type        = string
  default     = "mailroom-users"
}
