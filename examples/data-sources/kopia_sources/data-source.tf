# Every source the server knows, from every host.
data "kopia_sources" "all" {}

output "source_status" {
  value = { for s in data.kopia_sources.all.sources : s.target => s.status }
}

# The sources of one host, with when each one's next snapshot is due.
data "kopia_sources" "host1" {
  host = "host1"
}

output "host1_next_snapshot" {
  value = { for s in data.kopia_sources.host1.sources : s.path => s.next_snapshot_time }
}
