// Package retycclient wraps the `retyc` CLI binary as the CSI driver's only way to talk to the
// Retyc API. Dataroom creation is entangled with the CLI's AGE key/crypto stack (GetActiveKey,
// passphrase unlock) — that logic lives in retyc-cli's internal/service package, which this
// module cannot import (Go internal-package visibility), and is not something to reimplement
// against the raw HTTP API. Every call passes --json; retyc-cli prints a stable typed JSON
// payload on stdout on success and {"error": "..."} on stderr on failure (non-zero exit).
package retycclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// Client execs a retyc binary with a fixed set of environment variables (RETYC_TOKEN,
// RETYC_KEY_PASSPHRASE, ...) applied to every invocation.
type Client struct {
	// BinPath is the path to the retyc binary.
	BinPath string
	// Env holds extra environment variables (e.g. "RETYC_TOKEN=...",
	// "RETYC_KEY_PASSPHRASE=...") appended to the subprocess environment.
	Env []string
	// Timeout bounds every invocation. Zero means no timeout.
	Timeout time.Duration
}

// New returns a Client that execs binPath with env applied on every call.
func New(binPath string, env []string) *Client {
	return &Client{BinPath: binPath, Env: env, Timeout: 60 * time.Second}
}

// cliError wraps the {"error": "..."} payload retyc prints on stderr when --json is set and the
// command fails, or the raw stderr text when it isn't valid JSON (e.g. a crash before flag
// parsing).
type cliError struct {
	Args   []string
	Status string
	Raw    string
}

func (e *cliError) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("retyc %v: %s", e.Args, e.Status)
	}

	return fmt.Sprintf("retyc %v: %s", e.Args, e.Raw)
}

// run execs `retyc --json <args...>` and decodes stdout into out (skipped when out is nil).
func (c *Client) run(ctx context.Context, out any, args ...string) error {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	fullArgs := append([]string{"--json"}, args...)
	//nolint:gosec // G204: BinPath and args are controller-configured, not user input
	cmd := exec.CommandContext(ctx, c.BinPath, fullArgs...)
	cmd.Env = c.Env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var errPayload struct {
			Error string `json:"error"`
		}
		stderrText := stderr.String()
		if jsonErr := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &errPayload); jsonErr == nil && errPayload.Error != "" {
			return &cliError{Args: args, Status: errPayload.Error}
		}

		return &cliError{Args: args, Raw: stderrText}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("decoding retyc %v output: %w", args, err)
	}

	return nil
}

// DataroomCreateResult mirrors retyc's dataroomCreateJSON (cmd/dataroom.go).
type DataroomCreateResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CreateDataroom creates a new dataroom titled title (the CSI volume name — always unique, so it
// also becomes the dataroom's unambiguous WebDAV path component: /dataroom/<title>).
func (c *Client) CreateDataroom(ctx context.Context, title string) (*DataroomCreateResult, error) {
	var result DataroomCreateResult
	if err := c.run(ctx, &result, "dataroom", "create", "--title", title); err != nil {
		return nil, err
	}

	return &result, nil
}

// DataroomDeleteResult mirrors the anonymous JSON struct returned by `dataroom rm` on a root URI.
type DataroomDeleteResult struct {
	DataroomID   string `json:"dataroom_id"`
	URI          string `json:"uri"`
	DeletedCount int    `json:"deleted_count"`
}

// DeleteDataroom deletes the whole dataroom (root URI) identified by id. -y is mandatory
// alongside --json: retyc's confirm() refuses to prompt interactively when --json is set.
func (c *Client) DeleteDataroom(ctx context.Context, id string) (*DataroomDeleteResult, error) {
	var result DataroomDeleteResult
	uri := "retyc://" + id
	if err := c.run(ctx, &result, "dataroom", "rm", uri, "-y"); err != nil {
		return nil, err
	}

	return &result, nil
}

// Dataroom mirrors internal/api.Dataroom (only the fields exposed by `dataroom ls`).
type Dataroom struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// dataroomPage mirrors retyc's pagedJSON[api.Dataroom].
type dataroomPage struct {
	Items []Dataroom `json:"items"`
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}

// ListDatarooms returns every dataroom visible to the authenticated identity.
func (c *Client) ListDatarooms(ctx context.Context) ([]Dataroom, error) {
	var page dataroomPage
	if err := c.run(ctx, &page, "dataroom", "ls"); err != nil {
		return nil, err
	}

	return page.Items, nil
}

// UserQuota mirrors internal/api.UserQuota. Nil MaxCountDataroom/MaxCountShare means unlimited.
// There is no per-dataroom size cap in the API — only these account-wide totals.
type UserQuota struct {
	CountShare       int    `json:"count_share"`
	MaxCountShare    *int   `json:"max_count_share"`
	CountDataroom    int    `json:"count_dataroom"`
	MaxCountDataroom *int   `json:"max_count_dataroom"`
	UsedStorage      int64  `json:"used_storage"`
	MaxStorage       int64  `json:"max_storage"`
	IsUploadReadOnly bool   `json:"is_upload_read_only"`
}

// Quota returns the authenticated user's current quota usage/limits.
func (c *Client) Quota(ctx context.Context) (*UserQuota, error) {
	var q UserQuota
	if err := c.run(ctx, &q, "user", "quota"); err != nil {
		return nil, err
	}

	return &q, nil
}

// HasDataroomQuota reports whether one more dataroom can be created, and IsUploadReadOnly reports
// whether the account can no longer write at all — CreateVolume should surface both as
// ResourceExhausted rather than a generic error.
func (q *UserQuota) HasDataroomQuota() bool {
	return q.MaxCountDataroom == nil || q.CountDataroom < *q.MaxCountDataroom
}
