data "kopia_policies" "all" {}

output "policy_targets" {
  value = [for p in data.kopia_policies.all.policies : p.target]
}

# The policies of one user on one host, kopia-docker-watcher's path policies
# among them. The filters match exactly.
data "kopia_policies" "docker_host1" {
  host      = "host1"
  user_name = "docker"
}

# policy_json holds Kopia's own field names, for jsondecode.
output "docker_host1_cron" {
  value = {
    for p in data.kopia_policies.docker_host1.policies :
    p.target => try(jsondecode(p.policy_json).scheduling.cron, null)
  }
}
