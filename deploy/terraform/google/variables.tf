variable "project_id" {
  description = "Project id to create or use. Globally unique across Google Cloud."
  type        = string
}

variable "create_project" {
  description = "Create the project, or use an existing one with this id."
  type        = bool
  default     = true
}

variable "project_name" {
  description = "Display name, when creating the project."
  type        = string
  default     = "mailroom"
}

variable "billing_account" {
  description = <<-EOT
    Billing account id to attach. Optional: the Gmail API has a free quota and needs no
    billing account for ordinary use. Attach one only if you intend to add Pub/Sub push
    notifications later.
  EOT
  type        = string
  default     = ""
}

variable "org_id" {
  description = "Organization id, when creating the project inside one. Leave empty for a personal account."
  type        = string
  default     = ""
}

variable "public_url" {
  description = <<-EOT
    The URL your mailroom will be reachable at, exactly as MAILROOM_PUBLIC_URL will be set —
    scheme, host, no trailing slash. The redirect URIs are derived from it, and Google matches
    them character for character.

    Optional. Set it and Terraform will check, on every plan, whether the OAuth client is
    actually configured to work with it.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.public_url == "" || can(regex("^https?://[^/]+$", var.public_url))
    error_message = "public_url must be a bare origin such as https://mail.example.com, with no path or trailing slash."
  }
}

variable "oauth_client_id" {
  description = <<-EOT
    The client id from the OAuth client you created in the Console. Terraform cannot create
    that client — see the README for why — but given the id it can verify the client is set up
    correctly, which is the part people get wrong.

    Optional, and not a secret: a client id is public by design. The secret does not belong
    here and is not needed for the check.
  EOT
  type        = string
  default     = ""
}

variable "google_sign_in" {
  description = <<-EOT
    Whether this instance will use Google to sign operators in, as opposed to only linking
    Gmail mailboxes. It decides whether the second redirect URI is required — an instance
    signing in through Authentik or Keycloak does not need it.
  EOT
  type        = bool
  default     = true
}

variable "google_account" {
  description = <<-EOT
    The Google account that is supposed to own this project, as an email address. Terraform
    refuses to do anything if Application Default Credentials resolve to anybody else.

    This is required rather than optional because the failure it prevents is expensive and
    silent: ADC is a different credential from the one `gcloud config get-value account`
    reports, so a machine that has ever been pointed at a work account will quietly create a
    personal project inside the employer's organization. Naming the account you mean costs one
    line and makes that impossible.
  EOT
  type        = string

  validation {
    condition     = can(regex("^[^@[:space:]]+@[^@[:space:]]+$", var.google_account))
    error_message = "google_account must be an email address, such as you@example.com."
  }
}

