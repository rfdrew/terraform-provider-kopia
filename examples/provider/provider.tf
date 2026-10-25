terraform {
  required_providers {
    kopia = {
      source  = "rfdrew/kopia"
      version = "~> 0.1"
    }
  }
}

# Credentials are best supplied through the environment so they stay out of the
# configuration: KOPIA_SERVER_URL, KOPIA_SERVER_USERNAME and
# KOPIA_SERVER_PASSWORD.
provider "kopia" {
  url      = "https://kopia.example.com:51515"
  username = "kopia"
  password = var.kopia_password

  # The server generated its own certificate (`kopia server start
  # --tls-generate-cert`) and printed its fingerprint as "SERVER CERT SHA256".
  tls_cert_fingerprint = "cd50843049f0c6da48255e8022728d28124cb5b72fc3a2d81be496219c2cccf3"
}

variable "kopia_password" {
  description = "Password of the kopia server's UI user, as given to `kopia server start --server-password`."
  type        = string
  sensitive   = true
}
