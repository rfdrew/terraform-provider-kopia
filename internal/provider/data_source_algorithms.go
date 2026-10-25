package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &algorithmsDataSource{}
	_ datasource.DataSourceWithConfigure = &algorithmsDataSource{}
)

// NewAlgorithmsDataSource returns the kopia_algorithms data source.
func NewAlgorithmsDataSource() datasource.DataSource { return &algorithmsDataSource{} }

type algorithmsDataSource struct {
	client *kopiaclient.Client
}

type algorithmsEntryModel struct {
	ID         types.String `tfsdk:"id"`
	Deprecated types.Bool   `tfsdk:"deprecated"`
}

type algorithmsDataSourceModel struct {
	DefaultHash       types.String           `tfsdk:"default_hash"`
	DefaultEncryption types.String           `tfsdk:"default_encryption"`
	DefaultECC        types.String           `tfsdk:"default_ecc"`
	DefaultSplitter   types.String           `tfsdk:"default_splitter"`
	Hash              []algorithmsEntryModel `tfsdk:"hash"`
	Encryption        []algorithmsEntryModel `tfsdk:"encryption"`
	ECC               []algorithmsEntryModel `tfsdk:"ecc"`
	Splitter          []algorithmsEntryModel `tfsdk:"splitter"`
	Compression       []algorithmsEntryModel `tfsdk:"compression"`
}

func (d *algorithmsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_algorithms"
}

func (d *algorithmsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	list := func(description string) schema.ListNestedAttribute {
		return schema.ListNestedAttribute{
			Computed: true,
			MarkdownDescription: description + " Kopia lists the algorithms that are not deprecated first, " +
				"then the deprecated ones, each part sorted by `id`.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Name of the algorithm, as Kopia's settings and policies take it.",
					},
					"deprecated": schema.BoolAttribute{
						Computed: true,
						MarkdownDescription: "Whether Kopia has deprecated the algorithm. Only compression " +
							"algorithms are ever marked.",
					},
				},
			},
		}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the algorithms the Kopia server's release supports, as its UI offers them " +
			"for a new repository: hashing, encryption, error correction, splitting and compression. It describes " +
			"the server's Kopia release, not the repository it has open — `kopia_repository_status` reports " +
			"the algorithms the repository uses — and works without a repository connected.\n\n" +
			"Useful to check the names a policy uses, such as `compression.compressor_name` and " +
			"`splitter.algorithm` of `kopia_policy`. Policies also accept the compressor `none`, which is not " +
			"listed. `lz4` is listed as deprecated, but from Kopia 0.23 on it can neither compress nor " +
			"decompress anything; `zstd-best-compression` is deprecated too.",
		Attributes: map[string]schema.Attribute{
			"default_hash": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hash algorithm Kopia uses for a new repository unless told otherwise.",
			},
			"default_encryption": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Encryption algorithm Kopia uses for a new repository unless told otherwise.",
			},
			"default_ecc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Error-correction algorithm Kopia uses for a new repository when error " +
					"correction is turned on, which it is not by default.",
			},
			"default_splitter": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Splitter Kopia uses for a new repository unless told otherwise.",
			},
			"hash":       list("Hash algorithms."),
			"encryption": list("Encryption algorithms."),
			"ecc":        list("Error-correction algorithms."),
			"splitter": list("Splitters, which cut files into chunks. `FIXED` and `DYNAMIC` are older names of " +
				"`FIXED-4M` and `DYNAMIC-4M-BUZHASH`."),
			"compression": list("Compression algorithms. Kopia has no default compression: a policy turns it on."),
		},
	}
}

func (d *algorithmsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// algorithmsFromAPI maps one list in Kopia's order. A list Kopia leaves null
// is reported as empty, so that its length can always be taken.
func algorithmsFromAPI(algorithms []kopiaclient.AlgorithmInfo) []algorithmsEntryModel {
	models := make([]algorithmsEntryModel, 0, len(algorithms))
	for _, a := range algorithms {
		models = append(models, algorithmsEntryModel{ID: types.StringValue(a.ID), Deprecated: types.BoolValue(a.Deprecated)})
	}
	return models
}

func (d *algorithmsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	algorithms, err := d.client.Algorithms(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Kopia algorithms", repositoryCallErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &algorithmsDataSourceModel{
		DefaultHash:       types.StringValue(algorithms.DefaultHash),
		DefaultEncryption: types.StringValue(algorithms.DefaultEncryption),
		DefaultECC:        types.StringValue(algorithms.DefaultECC),
		DefaultSplitter:   types.StringValue(algorithms.DefaultSplitter),
		Hash:              algorithmsFromAPI(algorithms.Hash),
		Encryption:        algorithmsFromAPI(algorithms.Encryption),
		ECC:               algorithmsFromAPI(algorithms.ECC),
		Splitter:          algorithmsFromAPI(algorithms.Splitter),
		Compression:       algorithmsFromAPI(algorithms.Compression),
	})...)
}
