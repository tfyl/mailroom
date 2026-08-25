terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    # Only used to ask Google whether the OAuth client is configured correctly. Terraform
    # cannot create that client, so checking it is the most this configuration can do.
    external = {
      source  = "hashicorp/external"
      version = "~> 2.3"
    }
  }
}

# The provider takes the project id from the input variable rather than from the resource
# below. Reading it back off google_project would make the provider depend on something the
# provider itself creates, which Terraform rejects as a cycle.
provider "google" {
  project = var.project_id
}

# --- Which Google account is Terraform actually acting as? ---
#
# The provider authenticates with Application Default Credentials, and ADC has nothing to do
# with the account `gcloud config get-value account` reports. On a machine that has ever been
# pointed at a work account, the default ADC is that work account, and an apply intended for a
# personal project creates it inside the employer's organization instead — inheriting its IAM,
# its policies and its billing. That is not a hypothetical: it is how this configuration's
# project first came into existence, and it had to be deleted.
#
# Naming the expected account is therefore required rather than optional. A guard nobody sets
# is not a guard, and the cost of stating it once is a line in a tfvars file.

data "google_client_openid_userinfo" "terraform" {}

resource "terraform_data" "credentials_guard" {
  input = data.google_client_openid_userinfo.terraform.email

  lifecycle {
    precondition {
      condition = data.google_client_openid_userinfo.terraform.email == var.google_account
      error_message = join("", [
        "Terraform is authenticated as ${data.google_client_openid_userinfo.terraform.email}, ",
        "but google_account says this project belongs to ${var.google_account}. Nothing has ",
        "been created. Fix it by pointing Application Default Credentials at the right account ",
        "— export GOOGLE_APPLICATION_CREDENTIALS at a credentials file for it, or run ",
        "`gcloud auth application-default login` and pick it. `gcloud config set account` will ",
        "not help: the provider ignores the CLI account entirely, which is exactly why this ",
        "mistake is so easy to make.",
      ])
    }
  }
}

# Creating the project is optional. Point this at an existing one if you already have a
# project you would rather use.
resource "google_project" "mailroom" {
  count = var.create_project ? 1 : 0

  name            = var.project_name
  project_id      = var.project_id
  billing_account = var.billing_account != "" ? var.billing_account : null
  org_id          = var.org_id != "" ? var.org_id : null

  # Reading mail through the Gmail API does not require billing. A billing account is only
  # needed if you later add Pub/Sub push notifications.

  # PREVENT, not DELETE. This project holds the OAuth client every linked mailbox
  # authenticates through, and deleting it invalidates all of them at the provider — the
  # sealed credentials in the database become unopenable by Google, not merely stale.
  #
  # The path that makes this matter is documented and reasonable: create_project = false is
  # how the README says to point this at a project you already have. Applied against a state
  # that created one, that takes the count to zero and Terraform destroys it. So the honest
  # setting is the one that refuses. An operator who genuinely means to remove the project
  # does it in the Console, where Google asks a second time and offers thirty days to undo.
  deletion_policy = "PREVENT"

  # And refused by Terraform itself, because deletion_policy is the provider's opinion at
  # destroy time while this is core's at plan time — it fails before an apply exists to go
  # wrong. Removing a project genuinely meant to go is a deliberate edit here first.
  depends_on = [terraform_data.credentials_guard]

  lifecycle {
    # Refused by Terraform itself as well as by deletion_policy, because that is the
    # provider's opinion at destroy time while this is core's at plan time — it fails before
    # there is an apply to go wrong. Removing a project genuinely meant to go takes a
    # deliberate edit here first.
    prevent_destroy = true

    # An org-less project is a deliberate choice here, and the wrong-credentials failure this
    # configuration guards against is precisely one that lands the project inside an
    # organization. Stating the expectation means a run that would move it has to say so.
    postcondition {
      condition = self.org_id == (var.org_id != "" ? var.org_id : null)
      error_message = join("", [
        "The project was created under organization ${coalesce(self.org_id, "(none)")}, but org_id asked for ",
        var.org_id != "" ? var.org_id : "no organization at all",
        ". Google places a project in the authenticated account's default organization when ",
        "none is given, so this almost always means the credentials belong to a different ",
        "account than intended. Check the account named in google_account.",
      ])
    }
  }
}

# The Gmail API is the only service mailroom needs. Attachments, threads, labels, drafts,
# send and settings all live behind this one API.
resource "google_project_service" "gmail" {
  project = var.project_id
  service = "gmail.googleapis.com"

  # Leave the API enabled if this configuration is destroyed: disabling it would break any
  # mailbox still linked through credentials issued under it.
  disable_on_destroy = false

  depends_on = [google_project.mailroom, terraform_data.credentials_guard]
}

# --- Asking Google what it actually has ---
#
# Registering the URIs and knowing they are registered are separate jobs, and this is the
# second one. It reads Google's state directly rather than trusting anything this
# configuration believes it did, which is what makes it worth keeping now that the same
# question is also asked by the registration itself: a check that shares no code with the
# thing it checks is the only kind that can catch it lying.
#
# A check block rather than a precondition, deliberately. Data sources are read during plan,
# before the apply that registers anything, so on a first run this is describing the past. It
# has to be a warning that clears on the next plan, not a wall in front of the apply that
# clears it.

data "external" "oauth_client" {
  count = var.public_url != "" && var.oauth_client_id != "" ? 1 : 0

  program = ["sh", "${path.module}/scripts/check-redirect-uris.sh"]
  query = {
    client_id  = var.oauth_client_id
    public_url = var.public_url
  }
}

check "oauth_redirect_uris" {
  assert {
    condition = length(data.external.oauth_client) == 0 || data.external.oauth_client[0].result.linking != "missing"
    error_message = join("", [
      "Google does not have ${var.public_url}/accounts/link/google/callback registered on ",
      "this OAuth client, so linking a mailbox will fail with redirect_uri_mismatch. Set ",
      "it under Authorized redirect URIs on that client; this warning is ",
      "reporting the state Google is in now, before that apply has run.",
    ])
  }

  assert {
    condition = length(data.external.oauth_client) == 0 || !var.google_sign_in || data.external.oauth_client[0].result.login != "missing"
    error_message = join("", [
      "Google does not have ${var.public_url}/auth/google/callback registered on this OAuth ",
      "client, so signing in will fail with redirect_uri_mismatch. This is the one that gets ",
      "missed: it is a different path from the linking callback, so an instance can link ",
      "mailboxes perfectly and still be impossible to log into. Add it under Authorized ",
      "Terraform registers it on the next apply, or set google_sign_in = false if this ",
      "instance signs in through another issuer.",
    ])
  }

  assert {
    condition = length(data.external.oauth_client) == 0 || data.external.oauth_client[0].result.linking != "unknown_client"
    error_message = join("", [
      "Google does not recognise the client id in oauth_client_id, so nothing about the ",
      "redirect URIs could be checked. Confirm it against ",
      "https://console.cloud.google.com/auth/clients?project=${var.project_id} — a client id ",
      "ends in .apps.googleusercontent.com, and a client deleted and recreated gets a new one.",
    ])
  }

  assert {
    condition = length(data.external.oauth_client) == 0 || !contains(["refused"], data.external.oauth_client[0].result.linking)
    error_message = join("", [
      "Google refuses ${var.public_url}/accounts/link/google/callback outright, which is a ",
      "different problem from it being unregistered: the URI itself is one Google will not ",
      "accept. Check that public_url is https (Google requires it for anything but localhost) ",
      "and that the host is a real registrable domain.",
    ])
  }
}

locals {
  # mailroom derives both callbacks from MAILROOM_PUBLIC_URL, and Google compares redirect URIs
  # character for character, so these are the two exact strings that have to be registered.
  redirect_uris = concat(
    ["${var.public_url}/accounts/link/google/callback"],
    var.google_sign_in ? ["${var.public_url}/auth/google/callback"] : [],
  )
}
