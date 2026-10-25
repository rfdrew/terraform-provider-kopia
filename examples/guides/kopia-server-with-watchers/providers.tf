terraform {
  required_providers {
    kopia = {
      source  = "rfdrew/kopia"
      version = "~> 0.1"
    }
  }
}

# The Kopia server on host1. Both servers are connected to the same
# repository, so the policies and notification profiles written through this
# one apply to every host.
provider "kopia" {
  url                  = "https://host1.example.com:51515"
  password             = var.kopia_passwords["host1"]
  tls_cert_fingerprint = var.kopia_cert_fingerprints["host1"]
}

# The Kopia server on host2: for what each server keeps for itself, the
# throttle limits, and for the policy of its own sources, which it then
# applies at once instead of at its next refresh.
provider "kopia" {
  alias                = "host2"
  url                  = "https://host2.example.com:51515"
  password             = var.kopia_passwords["host2"]
  tls_cert_fingerprint = var.kopia_cert_fingerprints["host2"]
}

variable "kopia_passwords" {
  description = "Password of each server's UI user, as given to `kopia server start --server-password`."
  type        = map(string)
  sensitive   = true
}

variable "kopia_cert_fingerprints" {
  description = "SHA-256 fingerprint of each server's TLS certificate, as `kopia server start` prints it."
  type        = map(string)
}
