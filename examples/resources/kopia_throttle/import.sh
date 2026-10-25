# There is only one set of throttle limits, so the import id is always
# "throttle". Read then fills in the limits the server has in effect.
terraform import kopia_throttle.this throttle
