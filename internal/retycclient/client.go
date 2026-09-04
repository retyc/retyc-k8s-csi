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
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Client execs a retyc binary with a fixed environment applied to every invocation.
type Client struct {
	// BinPath is the path to the retyc binary.
	BinPath string
	// Env is the complete environment of the subprocess (it replaces, not appends to, the
	// parent's environment — exec.Cmd semantics). It must carry RETYC_TOKEN and
	// RETYC_KEY_PASSPHRASE; nil means "inherit the parent process environment".
	Env []string
	// Timeout bounds every invocation. Zero means no timeout.
	Timeout time.Duration
}

// New returns a Client that execs binPath with env applied on every call.
func New(binPath string, env []string) *Client {
	return &Client{BinPath: binPath, Env: env, Timeout: 60 * time.Second}
}

// Error is returned when the retyc CLI exits non-zero. Message is the {"error": "..."} payload
// retyc prints on stderr with --json, or the raw stderr text when no such payload is found
// (e.g. a crash before flag parsing).
type Error struct {
	Args    []string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("retyc %s: %s", strings.Join(e.Args, " "), e.Message)
}

// IsNotFound reports whether err is a retyc CLI error for a missing resource. The CLI has no
// structured error codes yet, so this matches the API error text the CLI relays
// (`API error 404: {"detail":"Dataroom not found"}`) — a documented POC-grade check.
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	msg := strings.ToLower(e.Message)

	return strings.Contains(msg, "api error 404") || strings.Contains(msg, "not found")
}

// parseStderrError extracts the {"error": "..."} payload from stderr. The CLI's spinner writes
// ANSI escape sequences to stderr even without a TTY (verified: `\r\x1b[K\x1b[36m⠋...` precedes
// the JSON on the same line), so a plain Unmarshal of the whole buffer fails — locate the JSON
// object instead of assuming stderr is clean.
func parseStderrError(stderr []byte) string {
	// Try every '{' from the first one on: the payload's own message may embed JSON (the API
	// error body, e.g. {"detail":"Dataroom not found"}), so the *last* '{' is usually wrong,
	// and the escape noise never contains '{' so the first candidate is normally right.
	for idx := bytes.IndexByte(stderr, '{'); idx >= 0; {
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(stderr[idx:]), &payload); err == nil && payload.Error != "" {
			return payload.Error
		}
		next := bytes.IndexByte(stderr[idx+1:], '{')
		if next < 0 {
			break
		}
		idx += 1 + next
	}

	return strings.TrimSpace(stripANSI(string(stderr)))
}

// stripANSI removes CSI escape sequences (ESC '[' params… final-byte), other two-byte ESC
// sequences, and carriage returns.
func stripANSI(s string) string {
	var b strings.Builder
	const (
		text = iota
		afterEsc
		inCSI
	)
	state := text
	for _, r := range s {
		switch state {
		case afterEsc:
			if r == '[' {
				state = inCSI
			} else {
				state = text // two-byte escape (ESC x): drop x and resume
			}
		case inCSI:
			if r >= 0x40 && r <= 0x7E { // final byte
				state = text
			}
		default:
			switch r {
			case 0x1b:
				state = afterEsc
			case '\r':
			default:
				b.WriteRune(r)
			}
		}
	}

	return b.String()
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
		msg := parseStderrError(stderr.Bytes())
		if msg == "" {
			msg = err.Error()
		}

		return &Error{Args: args, Message: msg}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("decoding retyc %s output: %w", strings.Join(args, " "), err)
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

// Dataroom mirrors internal/api.Dataroom (only the fields this driver needs).
type Dataroom struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// DataroomList is the result of ListDatarooms.
type DataroomList struct {
	Items []Dataroom
	// Complete is false when the account has more datarooms than the CLI returned: `retyc
	// dataroom ls` only ever fetches page 1 (service.ListDatarooms → client.ListDatarooms(ctx, 1))
	// and exposes no --page flag, so callers relying on Items for existence checks must treat an
	// incomplete list as "unknown", not "absent".
	Complete bool
}

// dataroomPage mirrors retyc's pagedJSON[api.Dataroom].
type dataroomPage struct {
	Items []Dataroom `json:"items"`
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}

// ListDatarooms returns the datarooms visible to the authenticated identity (first page only —
// see DataroomList.Complete).
func (c *Client) ListDatarooms(ctx context.Context) (*DataroomList, error) {
	var page dataroomPage
	if err := c.run(ctx, &page, "dataroom", "ls"); err != nil {
		return nil, err
	}

	return &DataroomList{Items: page.Items, Complete: page.Pages <= 1}, nil
}

// UserQuota mirrors internal/api.UserQuota. Nil MaxCountDataroom/MaxCountShare means unlimited.
// There is no per-dataroom size cap in the API — only these account-wide totals.
type UserQuota struct {
	CountShare       int   `json:"count_share"`
	MaxCountShare    *int  `json:"max_count_share"`
	CountDataroom    int   `json:"count_dataroom"`
	MaxCountDataroom *int  `json:"max_count_dataroom"`
	UsedStorage      int64 `json:"used_storage"`
	MaxStorage       int64 `json:"max_storage"`
	IsUploadReadOnly bool  `json:"is_upload_read_only"`
}

// Quota returns the authenticated user's current quota usage/limits.
func (c *Client) Quota(ctx context.Context) (*UserQuota, error) {
	var q UserQuota
	if err := c.run(ctx, &q, "user", "quota"); err != nil {
		return nil, err
	}

	return &q, nil
}

// HasDataroomQuota reports whether one more dataroom can be created.
func (q *UserQuota) HasDataroomQuota() bool {
	return q.MaxCountDataroom == nil || q.CountDataroom < *q.MaxCountDataroom
}

// AuthStatus mirrors retyc's authStatusJSON (cmd/output.go).
type AuthStatus struct {
	Authenticated bool   `json:"authenticated"`
	Offline       bool   `json:"offline"`
	Reason        string `json:"reason,omitempty"`
}

// AuthStatusCheck runs `retyc --json auth status`, which validates the configured token against
// the auth server (refreshing it if needed), and fails when the account is not authenticated.
// Note the CLI exits 0 either way — the verdict is in the JSON.
func (c *Client) AuthStatusCheck(ctx context.Context) error {
	var st AuthStatus
	if err := c.run(ctx, &st, "auth", "status"); err != nil {
		return err
	}
	if !st.Authenticated {
		return fmt.Errorf("retyc not authenticated: %s", st.Reason)
	}

	return nil
}
