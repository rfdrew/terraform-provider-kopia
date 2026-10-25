import {
  to = kopia_policy.docker_host1
  id = "docker@host1"
}

# The global policy, and a path policy; a Windows path needs its backslashes
# doubled in HCL: "Administrator@WIN1:C:\\Users".
import {
  to = kopia_policy.global
  id = "(global)"
}

import {
  to = kopia_policy.postgres
  id = "root@db1:/srv/postgres"
}
