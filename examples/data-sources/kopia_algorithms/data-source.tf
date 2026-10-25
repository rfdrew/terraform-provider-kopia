data "kopia_algorithms" "this" {}

locals {
  # The compressors a policy can use: those Kopia does not deprecate, plus
  # "none", which policies accept but Kopia does not list.
  kopia_compressors = concat(
    ["none"],
    [for c in data.kopia_algorithms.this.compression : c.id if !c.deprecated],
  )
}

output "kopia_compressors" {
  value = local.kopia_compressors
}

output "default_splitter" {
  value = data.kopia_algorithms.this.default_splitter # e.g. "DYNAMIC-4M-BUZHASH"
}
