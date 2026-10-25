# List resources need Terraform 1.14 or newer. Profiles the provider cannot
# manage, such as those of Kopia's testsender method, are left out with a
# warning that names them.
list "kopia_notification_profile" "all" {
  provider = kopia
}

# Reading each profile in full takes one request per profile, and puts its
# secrets (SMTP password, Pushover tokens, webhook URL and headers) in the
# results and in configuration generated from them.
list "kopia_notification_profile" "detailed" {
  provider         = kopia
  include_resource = true
}
