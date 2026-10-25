# Actions need Terraform 1.14 or newer. The action keeps no state, and every
# invocation delivers a real test message.
resource "kopia_notification_profile" "ops" {
  name         = "ops"
  min_severity = "warning"

  webhook = {
    endpoint = "https://ntfy.example.org/kopia-backups"
    headers = {
      Authorization = "Bearer ${var.ntfy_token}"
    }
  }

  # Test the profile once it is saved, and again after every change. The action
  # reads the profile's name, so it has to run after the profile is saved;
  # before_create would be a cycle.
  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.kopia_notification_test.ops]
    }
  }
}

action "kopia_notification_test" "ops" {
  config {
    name = kopia_notification_profile.ops.name
  }
}

# Any email, Pushover or webhook profile can be tested, including one set up in
# the Kopia UI, on demand with:
#
#   terraform apply -invoke=action.kopia_notification_test.mail
action "kopia_notification_test" "mail" {
  config {
    name = "mail"
  }
}

variable "ntfy_token" {
  description = "Access token of the ntfy topic."
  type        = string
  sensitive   = true
}
