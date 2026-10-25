package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &repositoryStatusDataSource{}
	_ datasource.DataSourceWithConfigure = &repositoryStatusDataSource{}
)

// NewRepositoryStatusDataSource returns the kopia_repository_status data
// source.
func NewRepositoryStatusDataSource() datasource.DataSource { return &repositoryStatusDataSource{} }

type repositoryStatusDataSource struct {
	client *kopiaclient.Client
}

type repositoryStatusDataSourceModel struct {
	Connected                  types.Bool   `tfsdk:"connected"`
	ConfigFile                 types.String `tfsdk:"config_file"`
	FormatVersion              types.Int64  `tfsdk:"format_version"`
	Hash                       types.String `tfsdk:"hash"`
	Encryption                 types.String `tfsdk:"encryption"`
	ECC                        types.String `tfsdk:"ecc"`
	ECCOverheadPercent         types.Int64  `tfsdk:"ecc_overhead_percent"`
	Splitter                   types.String `tfsdk:"splitter"`
	MaxPackSize                types.Int64  `tfsdk:"max_pack_size"`
	Storage                    types.String `tfsdk:"storage"`
	APIServerURL               types.String `tfsdk:"api_server_url"`
	SupportsContentCompression types.Bool   `tfsdk:"supports_content_compression"`
	Hostname                   types.String `tfsdk:"hostname"`
	Username                   types.String `tfsdk:"username"`
	Description                types.String `tfsdk:"description"`
	ReadOnly                   types.Bool   `tfsdk:"readonly"`
	EnableActions              types.Bool   `tfsdk:"enable_actions"`
	FormatBlobCacheDuration    types.String `tfsdk:"format_blob_cache_duration"`
}

func (d *repositoryStatusDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_repository_status"
}

func (d *repositoryStatusDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reports whether the Kopia server has a repository connected, and how: the " +
			"repository's format and algorithms, where it is stored, and the identity the server takes " +
			"snapshots as. It works without a repository too, and then reports only `connected = false`, with " +
			"every other attribute null.\n\n" +
			"Useful in a `check` block, to make sure the server is connected to the expected repository before " +
			"anything else is applied. The throttle limits the server started with are left out, because Kopia " +
			"reports them as they were at start-up; the `kopia_throttle` resource reads the limits in effect.",
		Attributes: map[string]schema.Attribute{
			"connected": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the server has a repository connected. Every other resource and " +
					"data source except `kopia_current_user` and `kopia_algorithms` needs one.",
			},
			"config_file": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Absolute path of the server's config file, `repository.config`, in which " +
					"Kopia saves the repository description and the throttle limits: " +
					"`/app/config/repository.config` for the official Docker image. Null for a repository reached " +
					"through a Kopia repository server.",
			},
			"format_version": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Format version of the repository: `1`, `2` or `3`, which Kopia uses for new repositories.",
			},
			"hash": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hash algorithm of the repository, such as `BLAKE2B-256-128`.",
			},
			"encryption": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Encryption algorithm of the repository, such as `AES256-GCM-HMAC-SHA256`.",
			},
			"ecc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Error-correction algorithm of the repository, such as `REED-SOLOMON-CRC32`, " +
					"or null when error correction is off. It is null whenever `ecc_overhead_percent` is `0`, " +
					"which is how Kopia itself decides that error correction is off: repositories created by old " +
					"releases may name an algorithm they do not use.",
			},
			"ecc_overhead_percent": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Share of the repository's storage spent on error correction, in percent; " +
					"`0` when error correction is off. Null for a repository reached through a Kopia repository " +
					"server.",
			},
			"splitter": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Splitter the repository cuts files into chunks with, such as `DYNAMIC-4M-BUZHASH`.",
			},
			"max_pack_size": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Largest size of a pack blob, in bytes: `20971520` (20 MiB) for new repositories.",
			},
			"storage": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Type of the repository's storage: `filesystem`, `s3`, `b2`, `gcs`, " +
					"`azureBlob`, `sftp`, `webdav`, `rclone` or `gdrive`.",
			},
			"api_server_url": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Address of the Kopia repository server through which the server reaches " +
					"its repository, or null when it is connected directly to the storage. With a repository " +
					"server, the repository's format, algorithms and storage are not reported, and those " +
					"attributes are null.",
			},
			"supports_content_compression": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the repository compresses data as it stores each content, which " +
					"needs index version 2 or later. Without it, Kopia compresses file contents as whole objects " +
					"instead.",
			},
			"hostname": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Host name the server takes snapshots as. A source is local to the server, " +
					"and snapshotted by it, when its host is this one. It is the repository connection's identity " +
					"(`--override-hostname` of `kopia repository connect`), which need not match the host name of " +
					"the machine that `kopia_current_user` reports.",
			},
			"username": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "User name the server takes snapshots as: the repository connection's " +
					"identity (`--override-username` of `kopia repository connect`), not the UI user the provider " +
					"signs in as.",
			},
			"description": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Description of the repository, as `kopia_repository_description` sets it; " +
					"`Repository in <storage>` unless it was changed.",
			},
			"readonly": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the repository is connected read-only, in which case the server " +
					"takes no snapshots and treats every source as remote.",
			},
			"enable_actions": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the server runs the actions — commands before and after " +
					"snapshots — that policies define. Kopia ignores them unless the repository was connected " +
					"with `--enable-actions`.",
			},
			"format_blob_cache_duration": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "How long the server caches the repository's format blob, as a Go duration " +
					"such as `15m0s`, Kopia's default. A duration of zero or less, such as `-1ns` for a " +
					"connection made with `--disable-repository-format-cache`, means it is not cached. Null when " +
					"the server reports none.",
			},
		},
	}
}

func (d *repositoryStatusDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// repositoryStatusFromAPI maps the status Kopia reports. Without a
// repository Kopia still sends the fields that have no omitempty, as zero
// values; those describe no repository at all, so everything but connected
// is null then, rather than false or "".
func repositoryStatusFromAPI(status *kopiaclient.RepoStatus) repositoryStatusDataSourceModel {
	m := repositoryStatusDataSourceModel{
		Connected:                  types.BoolValue(status.Connected),
		ConfigFile:                 types.StringNull(),
		FormatVersion:              types.Int64Null(),
		Hash:                       types.StringNull(),
		Encryption:                 types.StringNull(),
		ECC:                        types.StringNull(),
		ECCOverheadPercent:         types.Int64Null(),
		Splitter:                   types.StringNull(),
		MaxPackSize:                types.Int64Null(),
		Storage:                    types.StringNull(),
		APIServerURL:               types.StringNull(),
		SupportsContentCompression: types.BoolNull(),
		Hostname:                   types.StringNull(),
		Username:                   types.StringNull(),
		Description:                types.StringNull(),
		ReadOnly:                   types.BoolNull(),
		EnableActions:              types.BoolNull(),
		FormatBlobCacheDuration:    types.StringNull(),
	}
	if !status.Connected {
		return m
	}

	m.ConfigFile = stringValueOrNullIfEmpty(status.ConfigFile)
	m.FormatVersion = int64ValueOrNullIfZero(int64(status.FormatVersion))
	m.Hash = stringValueOrNullIfEmpty(status.Hash)
	m.Encryption = stringValueOrNullIfEmpty(status.Encryption)
	m.Splitter = stringValueOrNullIfEmpty(status.Splitter)
	m.MaxPackSize = int64ValueOrNullIfZero(status.MaxPackSize)
	m.Storage = stringValueOrNullIfEmpty(status.Storage)
	m.APIServerURL = stringValueOrNullIfEmpty(status.APIServerURL)
	m.SupportsContentCompression = types.BoolValue(status.SupportsContentCompression)
	m.Hostname = stringValueOrNullIfEmpty(status.Hostname)
	m.Username = stringValueOrNullIfEmpty(status.Username)
	m.Description = stringValueOrNullIfEmpty(status.Description)
	m.ReadOnly = types.BoolValue(status.ReadOnly)
	m.EnableActions = types.BoolValue(status.EnableActions)

	// Error correction is on exactly when the overhead is above 0, which is
	// the test Kopia's format code applies (repo/format/format_provider.go:116
	// in the releases); a repository created before Kopia cleared the
	// algorithm of an unused ECC may still name one. A repository reached through a
	// repository server reports neither, and 0 would wrongly say "off".
	if status.APIServerURL == "" {
		m.ECCOverheadPercent = types.Int64Value(int64(status.ECCOverheadPercent))
	}
	if status.ECCOverheadPercent > 0 {
		m.ECC = stringValueOrNullIfEmpty(status.ECC)
	}

	// Kopia sends the duration in nanoseconds; a Go duration string reads
	// better and is what the provider's request_timeout takes. Zero means
	// the field was left out: Kopia defaults it to 15 minutes for a direct
	// connection, and leaves it unset for one through a repository server.
	if status.FormatBlobCacheDuration != 0 {
		m.FormatBlobCacheDuration = types.StringValue(status.FormatBlobCacheDuration.String())
	}
	return m
}

func (d *repositoryStatusDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	status, err := d.client.RepoStatus(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia repository status", repositoryCallErrorDetail(err))
		return
	}

	state := repositoryStatusFromAPI(status)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
