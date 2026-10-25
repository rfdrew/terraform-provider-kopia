# The throttle limits of the Kopia server's repository connection. This is a
# singleton, so declare at most one. It owns all seven limits: a limit left
# out is unlimited, whatever the server had before.
#
# Kopia saves the limits in its config file and uses them again after a
# restart, so keep that file on persistent storage (a volume on /app/config
# for the official image). Destroying this resource sets every limit to
# unlimited and saves that.
resource "kopia_throttle" "this" {
  # Averages per second; Kopia lets up to a minute's worth through in a burst.
  reads_per_second  = 100
  writes_per_second = 50
  lists_per_second  = 10

  # Bytes, not bits, per second: 10 MiB/s up and 20 MiB/s down.
  max_upload_speed_bytes_per_second   = 10485760
  max_download_speed_bytes_per_second = 20971520

  concurrent_reads  = 16
  concurrent_writes = 8
}
