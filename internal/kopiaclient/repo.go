package kopiaclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RepoStatus mirrors serverapi.StatusResponse (internal/serverapi/
// serverapi.go:21-40), the body of GET /api/v1/repo/status and of POST
// /api/v1/repo/description. It embeds repo.ClientOptions (repo/
// local_config.go:28-43), whose fields Go's encoding/json promotes to the top
// level, so they appear here as plain fields.
//
// Only the fields without omitempty are always present. A server with no
// repository open sends just "connected": false and the zero values of the
// others; a server connected to a remote Kopia repository server rather than
// directly to storage leaves out configFile, hash, encryption, ecc,
// splitter, maxPackSize and storage, and may leave out description and
// formatBlobCacheDuration (spec: repo.md §2).
type RepoStatus struct {
	// Connected is the readiness signal: every API route other than a few
	// status endpoints answers NOT_CONNECTED while it is false.
	Connected bool `json:"connected"`
	// ConfigFile is the absolute path of the server's repository.config,
	// where the description and the throttle limits are saved.
	ConfigFile string `json:"configFile,omitempty"`
	// FormatVersion is the repository format version: 1, 2 or 3.
	FormatVersion int    `json:"formatVersion,omitempty"`
	Hash          string `json:"hash,omitempty"`
	Encryption    string `json:"encryption,omitempty"`
	// ECC names the error-correction algorithm. ECC is enabled exactly when
	// ECCOverheadPercent is above 0: a repository created by an old Kopia may
	// name an algorithm with an overhead of 0.
	ECC                string `json:"ecc,omitempty"`
	ECCOverheadPercent int    `json:"eccOverheadPercent,omitempty"`
	Splitter           string `json:"splitter,omitempty"`
	// MaxPackSize is in bytes.
	MaxPackSize int64 `json:"maxPackSize,omitempty"`
	// Storage is the storage type, such as filesystem or s3.
	Storage string `json:"storage,omitempty"`
	// APIServerURL is set instead of the storage fields when the server
	// reaches its repository through another Kopia server.
	APIServerURL               string `json:"apiServerURL,omitempty"`
	SupportsContentCompression bool   `json:"supportsContentCompression"`

	// Hostname and Username are the connection identity the server takes
	// snapshots as, which decides whether a source is local to it. They are
	// not the operating system's user and host; see CurrentUser.
	Hostname               string `json:"hostname"`
	Username               string `json:"username"`
	ReadOnly               bool   `json:"readonly,omitempty"`
	PermissiveCacheLoading bool   `json:"permissiveCacheLoading,omitempty"`
	Description            string `json:"description,omitempty"`
	EnableActions          bool   `json:"enableActions"`
	// FormatBlobCacheDuration is sent as a count of nanoseconds, which is
	// how encoding/json writes a time.Duration and how it decodes one back.
	// Kopia defaults it to 15 minutes; -1ns, or any value of 0 or below,
	// means the format blob is not cached.
	FormatBlobCacheDuration time.Duration `json:"formatBlobCacheDuration,omitempty"`
	// ThrottlingLimits is the value the config file held when the
	// repository was opened. PUT /api/v1/repo/throttle does not update it,
	// so it is stale as soon as the limits change; GetThrottle reads the
	// limits in effect.
	ThrottlingLimits *ThrottleLimits `json:"throttlingLimits,omitempty"`

	// InitTaskID names the task opening, connecting or creating the
	// repository while one runs.
	InitTaskID string `json:"initTaskID,omitempty"`
}

// RepoStatus reads GET /api/v1/repo/status. It works without a repository
// open, reporting Connected false then, and never fails in the handler.
func (c *Client) RepoStatus(ctx context.Context) (*RepoStatus, error) {
	var status RepoStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/repo/status", nil, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// AlgorithmInfo mirrors serverapi.AlgorithmInfo: one algorithm the server
// binary supports.
type AlgorithmInfo struct {
	ID         string `json:"id"`
	Deprecated bool   `json:"deprecated"`
}

// SupportedAlgorithms mirrors serverapi.SupportedAlgorithmsResponse
// (serverapi.go:135-153), the body of GET /api/v1/repo/algorithms. It
// describes what the server binary can use when it creates a repository, not
// the repository it has open, and has no default for compression. Each list
// holds the non-deprecated algorithms first, then the deprecated ones, each
// part sorted by id (internal/server/api_repo.go:291-300). Kopia marks only
// compression algorithms as deprecated.
type SupportedAlgorithms struct {
	DefaultHash       string          `json:"defaultHash"`
	DefaultEncryption string          `json:"defaultEncryption"`
	DefaultECC        string          `json:"defaultEcc"`
	DefaultSplitter   string          `json:"defaultSplitter"`
	Hash              []AlgorithmInfo `json:"hash"`
	Encryption        []AlgorithmInfo `json:"encryption"`
	ECC               []AlgorithmInfo `json:"ecc"`
	Splitter          []AlgorithmInfo `json:"splitter"`
	Compression       []AlgorithmInfo `json:"compression"`
}

// Algorithms reads GET /api/v1/repo/algorithms, which works without a
// repository open.
func (c *Client) Algorithms(ctx context.Context) (*SupportedAlgorithms, error) {
	var algorithms SupportedAlgorithms
	if err := c.do(ctx, http.MethodGet, "/api/v1/repo/algorithms", nil, nil, &algorithms); err != nil {
		return nil, err
	}
	return &algorithms, nil
}

// ThrottleLimits mirrors throttling.Limits (repo/blob/throttling/
// throttler.go:142-150), field for field and with the same omitempty tags.
// Zero means unlimited, and Kopia leaves zero fields out, so "no limits at
// all" is {}.
//
// The rates are averages: each is a token bucket holding 60 seconds' worth,
// so a burst may briefly exceed it. Upload and download are in bytes, not
// bits, per second.
type ThrottleLimits struct {
	ReadsPerSecond         float64 `json:"readsPerSecond,omitempty"`
	WritesPerSecond        float64 `json:"writesPerSecond,omitempty"`
	ListsPerSecond         float64 `json:"listsPerSecond,omitempty"`
	UploadBytesPerSecond   float64 `json:"maxUploadSpeedBytesPerSecond,omitempty"`
	DownloadBytesPerSecond float64 `json:"maxDownloadSpeedBytesPerSecond,omitempty"`
	ConcurrentReads        int64   `json:"concurrentReads,omitempty"`
	ConcurrentWrites       int64   `json:"concurrentWrites,omitempty"`
}

// The two ways PUT /api/v1/repo/throttle fails with MALFORMED_REQUEST mean
// opposite things, so SetThrottle wraps the *Error in one of these. Test with
// errors.Is; errors.As still finds the *Error.
var (
	// ErrThrottleRejected means Kopia refused the limits and kept the ones
	// it had: the body did not decode, or a limit was out of range.
	ErrThrottleRejected = errors.New("kopia rejected the throttle limits and kept the previous ones")

	// ErrThrottleNotPersisted means Kopia applied the limits but could not
	// save them to its config file. They are in effect until the server
	// restarts, and the saved ones come back then.
	ErrThrottleNotPersisted = errors.New("kopia applied the throttle limits but could not save them to its config file")
)

// throttleLimitFields are the names SetLimits puts in front of a validation
// error (throttler.go:105-133). They are the Go field names of
// throttling.Limits, which differ from the JSON keys for the two speeds.
var throttleLimitFields = []string{
	"ReadsPerSecond",
	"WritesPerSecond",
	"ListsPerSecond",
	"UploadBytesPerSecond",
	"DownloadBytesPerSecond",
	"ConcurrentReads",
	"ConcurrentWrites",
}

// throttleSetLimitsPrefix starts every error the handler reports from
// SetLimits (internal/server/api_repo.go:322-324).
const throttleSetLimitsPrefix = "unable to set limits: "

// GetThrottle reads GET /api/v1/repo/throttle: the limits in effect, held in
// memory. They are what the last PUT set or, on a server that never received
// one, what the config file held at start-up, or the repository's storage
// configuration (--max-upload-speed and --max-download-speed of `kopia
// repository connect`) when the file holds none.
//
// It needs a repository connected directly to its storage: NOT_CONNECTED
// without one, STORAGE_CONNECTION for a connection through another Kopia
// server (see IsStorageConnection).
func (c *Client) GetThrottle(ctx context.Context) (*ThrottleLimits, error) {
	var limits ThrottleLimits
	if err := c.do(ctx, http.MethodGet, "/api/v1/repo/throttle", nil, nil, &limits); err != nil {
		return nil, err
	}
	return &limits, nil
}

// SetThrottle replaces all seven limits with PUT /api/v1/repo/throttle. It is
// not a merge: a zero field, left out of the body, becomes unlimited, so
// SetThrottle(ctx, ThrottleLimits{}) sends {} and removes every limit.
//
// Kopia applies the limits, then saves them as "throttlingLimits" in the
// server's repository.config, where the next start picks them up again (spec:
// repo.md §4.5). Both steps report failure as 400 MALFORMED_REQUEST, so the
// error is wrapped in ErrThrottleRejected when nothing changed, and in
// ErrThrottleNotPersisted when the limits took effect but were not saved.
// Callers that also write the description should hold LockConfigFile.
func (c *Client) SetThrottle(ctx context.Context, limits ThrottleLimits) error {
	err := c.do(ctx, http.MethodPut, "/api/v1/repo/throttle", nil, limits, nil)
	if err == nil {
		return nil
	}
	apiErr, ok := asError(err)
	if !ok || apiErr.Code != CodeMalformedRequest {
		return err
	}
	// The body is decoded before anything is applied
	// (api_repo.go:318-320); SetLimits then validates every field and puts
	// the previous limits back when one fails (throttler.go:85-92), so in
	// both cases nothing changed. Any other text after the prefix comes from
	// the hook that saves the config file, which runs after the new limits
	// took effect (throttler.go:94-100).
	if !strings.HasPrefix(apiErr.Message, throttleSetLimitsPrefix) {
		return fmt.Errorf("%w: %w", ErrThrottleRejected, err)
	}
	cause := strings.TrimPrefix(apiErr.Message, throttleSetLimitsPrefix)
	for _, field := range throttleLimitFields {
		if strings.HasPrefix(cause, field+":") {
			return fmt.Errorf("%w: %w", ErrThrottleRejected, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrThrottleNotPersisted, err)
}

// IsStorageConnection reports whether Kopia answered STORAGE_CONNECTION,
// which the throttle endpoints do when the server reaches its repository
// through another Kopia server instead of directly on its storage: only a
// direct connection has throttle limits.
func IsStorageConnection(err error) bool {
	apiErr, ok := asError(err)
	return ok && apiErr.Code == CodeStorageConnection
}

// SetDescription sets the repository description with POST
// /api/v1/repo/description and returns the repository status Kopia answers
// with, which carries the new description.
//
// Kopia keeps the description in the server's repository.config, not in the
// repository. To save it, the handler writes back the whole client
// configuration the repository was opened with, so the file's
// throttlingLimits revert to their value at start-up although the limits in
// effect do not change (spec: repo.md §5.3). Callers should hold
// LockConfigFile and put the limits from GetThrottle back with SetThrottle
// afterwards.
//
// An empty description is saved as no description at all, and the next start
// replaces it with Kopia's default, "Repository in <storage>".
func (c *Client) SetDescription(ctx context.Context, description string) (*RepoStatus, error) {
	// The handler decodes a whole repo.ClientOptions and uses only its
	// description, so that is all the body needs.
	body := struct {
		Description string `json:"description"`
	}{Description: description}

	var status RepoStatus
	if err := c.do(ctx, http.MethodPost, "/api/v1/repo/description", nil, body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// CurrentUser mirrors serverapi.CurrentUserResponse (serverapi.go:224-228).
// It names the operating-system account the kopia server process runs as and
// the machine's host name, lower-cased and cut at the first dot
// (repo/userhost.go:12-43). It is neither the UI user the client signs in as
// nor the connection identity of RepoStatus.
type CurrentUser struct {
	Username string `json:"username"`
	Hostname string `json:"hostname"`
}

// CurrentUser reads GET /api/v1/current-user, which works without a
// repository open.
func (c *Client) CurrentUser(ctx context.Context) (*CurrentUser, error) {
	var user CurrentUser
	if err := c.do(ctx, http.MethodGet, "/api/v1/current-user", nil, nil, &user); err != nil {
		return nil, err
	}
	return &user, nil
}
