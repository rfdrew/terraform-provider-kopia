data "kopia_notification_profiles" "all" {}

# Names, to import a profile set up in the Kopia UI or with the CLI.
output "notification_profiles" {
  value = { for profile in data.kopia_notification_profiles.all.profiles : profile.name => profile.method }
}

# Make sure failed snapshots do not go unnoticed.
check "kopia_notifies_errors" {
  assert {
    condition     = length(data.kopia_notification_profiles.all.profiles) > 0
    error_message = "The Kopia server has no notification profile, so nobody hears about failed snapshots."
  }
}
