# Policies are imported by their target, written exactly as Kopia writes it:
# `kopia policy list` and the kopia_policies data source show it. Quote it, as
# it may contain characters the shell interprets.
terraform import kopia_policy.global '(global)'
terraform import kopia_policy.docker_host1 'docker@host1'
terraform import kopia_policy.postgres 'root@db1:/srv/postgres'

# Kopia cannot store a plain false, such as scheduling.manual = false, or an
# empty section, such as retention = {}; the first plan after an import shows
# those as an update that changes nothing on the server.
