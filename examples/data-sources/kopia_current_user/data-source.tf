data "kopia_current_user" "this" {}

# The operating-system account and host the server process runs as. The
# identity the server takes snapshots as is reported by kopia_repository_status
# instead, and may differ.
output "kopia_server_process" {
  value = "${data.kopia_current_user.this.username}@${data.kopia_current_user.this.hostname}" # e.g. "root@kopia"
}
