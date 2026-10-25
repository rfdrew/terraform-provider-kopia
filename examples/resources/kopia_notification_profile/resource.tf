# A webhook to an ntfy topic. Kopia counts only HTTP 200 as delivered: ntfy
# answers 200, but a service that answers 201, 202 or 204 makes every
# notification fail.
resource "kopia_notification_profile" "ntfy" {
  name         = "ntfy"
  min_severity = "warning"

  webhook = {
    endpoint = "https://ntfy.example.org/kopia-backups"
    headers = {
      Authorization = "Bearer ${var.ntfy_token}"
      Priority      = "high"
    }
  }

  # Have Kopia send its test notification after every apply that creates or
  # changes the profile, so that a wrong token fails the apply rather than the
  # next real report. Every test delivers a real message.
  send_test_on_apply = true
}

# Email through a submission server. Kopia connects without encryption and
# switches to TLS with STARTTLS when the server offers it; implicit TLS on port
# 465 does not work.
resource "kopia_notification_profile" "mail" {
  name = "mail"

  email = {
    smtp_server   = "smtp.example.com"
    smtp_port     = 587
    smtp_username = "kopia@example.com"
    smtp_password = var.smtp_password
    from          = "kopia@example.com"
    # Bare addresses separated by commas, without spaces. Kopia ignores `cc`,
    # so every recipient goes here.
    to = "ops@example.com,oncall@example.com"
  }
}

# Pushover, for errors only. `format` is "txt" (the default here) or "html";
# Kopia accepts no other value, "md" included.
resource "kopia_notification_profile" "phone" {
  name         = "phone"
  min_severity = "error"

  pushover = {
    app_token = var.pushover_app_token
    user_key  = var.pushover_user_key
    format    = "html"
  }
}

variable "ntfy_token" {
  type      = string
  sensitive = true
}

variable "smtp_password" {
  type      = string
  sensitive = true
}

variable "pushover_app_token" {
  type      = string
  sensitive = true
}

variable "pushover_user_key" {
  type      = string
  sensitive = true
}
