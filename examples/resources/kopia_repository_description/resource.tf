# The description the Kopia UI shows for the repository. This is a singleton,
# so declare at most one. Kopia saves it in the server's config file, not in
# the repository, and destroying this resource leaves it in place.
resource "kopia_repository_description" "this" {
  description = "Production backups (managed by Terraform)"
}
