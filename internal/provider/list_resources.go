package provider

import (
	"iter"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
)

// List resources answer `terraform query` (Terraform 1.14 and later): a list
// block in a .tfquery.hcl file names a resource type, and the provider reports
// every object of that type it can find, each with its resource identity, a
// display name and — when the block sets include_resource — the attributes
// Terraform would hold after importing it and refreshing it. `terraform query
// -generate-config-out` turns the results into import blocks and resource
// configuration.
//
// Only resources with an identity can be listed, which leaves out the
// singletons kopia_throttle and kopia_repository_description (see
// resource_identity.go): there would be one result each, with nothing to
// identify it by.
//
// Kopia can hold objects the resources cannot represent: policies of targets
// the strict target parser refuses, and notification profiles of a method the
// provider does not manage. A list leaves them out rather than failing on
// them, with a warning naming them, because one odd object written by another
// client should not hide all the others.

// listLimitReached reports whether a list has already produced as many results
// as Terraform asked for. A limit of 0 sets none.
func listLimitReached(req list.ListRequest, produced int) bool {
	return req.Limit > 0 && int64(produced) >= req.Limit
}

// listError is a result stream that carries nothing but one error.
func listError(summary, detail string) iter.Seq[list.ListResult] {
	var diags diag.Diagnostics
	diags.AddError(summary, detail)
	return list.ListResultsStreamDiagnostics(diags)
}

// unconfiguredListError is the error for a list run before the provider was
// configured, which Terraform never does.
func unconfiguredListError() iter.Seq[list.ListResult] {
	return listError(
		"Unconfigured Kopia provider",
		"The list ran without a configured provider. This is a bug in the provider.",
	)
}

// listEmitter pushes the results of a list, counting them against the limit,
// and carries the warnings about objects the list leaves out.
//
// A warning travels with the next result pushed, rather than in a result of
// its own: the framework passes a result that holds nothing but warnings on to
// Terraform, but a result with an identity is what every Terraform release
// that has list resources is certain to accept. Warnings that no result
// follows are pushed on their own by flush, as there is nothing left to carry
// them.
type listEmitter struct {
	req      list.ListRequest
	push     func(list.ListResult) bool
	produced int
	pending  diag.Diagnostics
}

func newListEmitter(req list.ListRequest, push func(list.ListResult) bool) *listEmitter {
	return &listEmitter{req: req, push: push}
}

// warn records a warning for the next result.
func (e *listEmitter) warn(summary, detail string) {
	e.pending.AddWarning(summary, detail)
}

// full reports whether the list has produced as many results as Terraform
// asked for.
func (e *listEmitter) full() bool {
	return listLimitReached(e.req, e.produced)
}

// emit pushes result together with the pending warnings, and reports whether
// Terraform wants more results.
func (e *listEmitter) emit(result list.ListResult) bool {
	diags := append(diag.Diagnostics{}, e.pending...)
	diags.Append(result.Diagnostics...)
	result.Diagnostics = diags
	e.pending = nil
	e.produced++
	return e.push(result)
}

// flush pushes the warnings no result carried, in a result of their own.
func (e *listEmitter) flush() {
	if len(e.pending) == 0 {
		return
	}
	warnings := e.pending
	e.pending = nil
	e.push(list.ListResult{Diagnostics: warnings})
}
