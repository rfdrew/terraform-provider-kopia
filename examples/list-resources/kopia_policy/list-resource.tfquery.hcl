# List resources need Terraform 1.14 or newer. Run `terraform query` to see the
# targets that have a policy, or `terraform query -generate-config-out=policies.tf`
# to write an import block and a kopia_policy block for each one.
list "kopia_policy" "all" {
  provider = kopia

  # Report every setting of each policy, not just its target, so the generated
  # resource blocks are complete. The listing holds every policy already, so
  # this costs no further request.
  include_resource = true
}

# The filters match exactly. This lists the policies of user docker on host1:
# its user policy and the path policies below it. Where kopia-docker-watcher
# keeps those path policies in line with container labels, leave them to it.
list "kopia_policy" "docker_host1" {
  provider = kopia

  config {
    host      = "host1"
    user_name = "docker"
  }
}
