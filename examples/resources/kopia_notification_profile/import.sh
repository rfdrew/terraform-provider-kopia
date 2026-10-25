# Notification profiles are imported by name, as `kopia notification profile
# list` or the kopia_notification_profiles data source shows it. Names are
# case-sensitive. Kopia returns every setting, secrets included, so nothing is
# lost on import, and the first plan shows any difference from the
# configuration.
terraform import kopia_notification_profile.ntfy ntfy
