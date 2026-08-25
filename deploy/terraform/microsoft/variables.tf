variable "tenant_id" {
  description = <<-EOT
    The Entra directory (tenant) to create the registration in, as a GUID or an
    `*.onmicrosoft.com` domain.

    Required rather than inferred. The provider would otherwise fall back to the signed-in
    CLI account's default tenant, and for a personal Microsoft account that is Microsoft's
    shared consumer directory — which cannot hold an app registration at all. `az account
    list` shows the directories an account can actually use; an account with none of its own
    needs one creating first, which is free and needs no Azure subscription.
  EOT
  type        = string

  validation {
    condition     = trimspace(var.tenant_id) != ""
    error_message = "tenant_id is required. `az account list --query \"[].{tenant:tenantId,domain:tenantDefaultDomain}\"` lists the directories you can use."
  }

  validation {
    condition     = var.tenant_id != "9188040d-6c67-4c5b-b112-36a304b66dad"
    error_message = "That is Microsoft's shared consumer directory, which every personal Microsoft account belongs to and which cannot hold an app registration. Create a directory of your own — free, and no Azure subscription needed — and name that one instead."
  }
}

variable "public_url" {
  description = <<-EOT
    The URL your mailroom is reachable at, exactly as MAILROOM_PUBLIC_URL is set — scheme,
    host, no trailing slash. The redirect URI is derived from it and Entra matches it
    character for character.
  EOT
  type        = string

  validation {
    condition     = can(regex("^https?://[^/]+$", var.public_url))
    error_message = "public_url must be a bare origin such as https://mail.example.com, with no path or trailing slash."
  }

  validation {
    condition     = can(regex("^https://", var.public_url)) || can(regex("^http://localhost(:[0-9]+)?$", var.public_url))
    error_message = "Entra requires https for redirect URIs, with http allowed only for localhost."
  }
}

variable "display_name" {
  description = "Name the registration appears under in the Entra portal, and on the consent screen the person linking a mailbox sees."
  type        = string
  default     = "mailroom"
}

variable "sign_in_audience" {
  description = <<-EOT
    Who may consent to this registration.

    `AzureADandPersonalMicrosoftAccount` — work, school and personal accounts. Pairs with
    mailroom's default MAILROOM_MICROSOFT_TENANT=common, and is the right answer unless you
    know otherwise.

    `AzureADMyOrg` — this directory only. If you choose it you must also set
    MAILROOM_MICROSOFT_TENANT to this tenant id, because `common` is refused with
    AADSTS50194 at consent rather than at apply.

    The two multitenant-without-personal and personal-only values are deliberately not offered:
    neither buys anything here that these two do not.
  EOT
  type        = string
  default     = "AzureADandPersonalMicrosoftAccount"

  validation {
    condition     = contains(["AzureADandPersonalMicrosoftAccount", "AzureADMyOrg"], var.sign_in_audience)
    error_message = "sign_in_audience must be AzureADandPersonalMicrosoftAccount or AzureADMyOrg."
  }
}

variable "create_client_secret" {
  description = <<-EOT
    Generate the client secret here. The value then exists in Terraform state, which is only
    acceptable if that state is stored somewhere you would keep a password.

    Set false to create the secret by hand in the portal instead, under Certificates &
    secrets on the registration this creates. Everything else still applies.
  EOT
  type        = bool
  default     = true
}

variable "secret_end_date" {
  description = <<-EOT
    When the generated client secret expires, as an RFC3339 timestamp such as
    2027-01-01T00:00:00Z. Empty takes Entra's own default of two years.

    A directory whose policy caps app secret lifetimes shorter than this refuses it at apply
    rather than quietly shortening it.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.secret_end_date == "" || can(formatdate("YYYY-MM-DD", var.secret_end_date))
    error_message = "secret_end_date must be an RFC3339 timestamp, such as 2027-01-01T00:00:00Z."
  }
}
