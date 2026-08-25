output "project_id" {
  description = "Project the Gmail API is enabled on"
  value       = var.project_id
}

output "next_step" {
  description = "The part that still has no API"
  value       = <<-EOT
    The Gmail API is enabled on ${var.project_id}.

    Google publishes no API that can create an OAuth consent screen or an OAuth client, so
    that much is still a Console step:

      1. https://console.cloud.google.com/auth/overview?project=${var.project_id}
         Configure the consent screen. Choose External unless everyone using this instance is
         in the same Google Workspace organization. While the app is in Testing, add every
         mailbox you intend to link as a test user, or linking fails with access_denied.

      2. https://console.cloud.google.com/auth/clients?project=${var.project_id}
         Create an OAuth client, type "Web application". Leave the redirect URIs empty.

    Then add the two authorized redirect URIs on that client. Google exposes no API for
    this — not Terraform, not gcloud, not any REST surface — so it is a Console step:

        <MAILROOM_PUBLIC_URL>/accounts/link/google/callback
        <MAILROOM_PUBLIC_URL>/auth/google/callback

    The first links a Gmail mailbox; the second signs you in. They are different paths and
    Google matches exactly, so registering only the first produces an instance that links
    mailboxes fine and cannot be logged into. Skip the second if this instance signs in
    through another issuer.

    Terraform cannot register them, but it does check them. Set public_url and
    oauth_client_id and every plan asks Google whether each one is there, so a missed step is
    a warning on the next plan rather than a broken sign-in later.

  EOT
}
