data "kopia_repository_status" "this" {}

output "repository_storage" {
  value = data.kopia_repository_status.this.storage # e.g. "filesystem"
}

# Make sure the server has the expected repository open before anything else
# relies on it.
check "kopia_repository_connected" {
  assert {
    condition     = data.kopia_repository_status.this.connected
    error_message = "The Kopia server has no repository connected."
  }

  assert {
    condition     = data.kopia_repository_status.this.config_file == "/app/config/repository.config"
    error_message = "The Kopia server keeps its config file somewhere other than the volume on /app/config."
  }
}
