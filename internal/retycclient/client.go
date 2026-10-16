// Package retycclient wraps the `retyc` CLI binary as the CSI driver's only way to talk to the
// Retyc API. Dataroom creation is entangled with the CLI's AGE key/crypto stack (GetActiveKey,
// passphrase unlock) — that logic lives in retyc-cli's internal/service package, which this
// module cannot import (Go internal-package visibility), and is not something to reimplement
// against the raw HTTP API. Every call passes --json; retyc-cli prints a stable typed JSON
// payload on stdout on success and a JSON error object on stderr on failure (non-zero exit):
// {"error": "...", "kind": "...", "status": 400, "detail": "..."} since retyc-cli 17e7230, only
// {"error": "..."} before it. The kind is what this package branches on; an older CLI falls back
// to the text of the error.
package retycclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/retyc/retyc-k8s-csi/internal/identity"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
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

// Error is returned when the retyc CLI exits non-zero. Message is the "error" of the JSON object
// retyc prints on stderr with --json, or the raw stderr text when no such object is found (e.g. a
// crash before flag parsing).
type Error struct {
	Args    []string
	Message string
	// Kind is the CLI's stable classification of the error (the Kind* constants), "" when stderr
	// carried none: a CLI older than the structured errors, or no JSON at all. The Is* predicates
	// branch on it first and fall back to the text of Message only when it is empty.
	Kind string
	// Status is the HTTP status of an API refusal, 0 for any other error or an older CLI.
	Status int
	// Detail is the API's own wording of a refusal: informative only, it may change.
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("retyc %s: %s", strings.Join(e.Args, " "), e.Message)
}

// The kinds of the CLI's JSON errors this driver tells apart (doc/commands.md of retyc-cli, "Errors
// in JSON"). A kind is never renamed nor given another meaning; one unknown here matches none of
// the predicates below, like the CLI's generic "error". lost_writes is left out: the mount pods
// run without --json, and their exit code says it (75).
const (
	KindAuthRequired     = "auth_required"
	KindConfig           = "config"
	KindRateLimited      = "rate_limited"
	KindStorageExceeded  = "storage_exceeded"
	KindPlanExceeded     = "plan_exceeded"
	KindCapacityTooSmall = "capacity_too_small"
	KindCapacityShrink   = "capacity_shrink"
	KindUnauthenticated  = "unauthenticated"
	KindForbidden        = "forbidden"
	KindNotFound         = "not_found"
	KindGone             = "gone"
	KindInvalid          = "invalid"
	KindCLITooOld        = "cli_too_old"
)

// classify returns the kind of a CLI error and its lowercased message, false when err is not one.
// The kind is "" when the CLI gave none: the caller then reads the message.
func classify(err error) (kind, msg string, ok bool) {
	var e *Error
	if !errors.As(err, &e) {
		return "", "", false
	}

	return e.Kind, strings.ToLower(e.Message), true
}

// hasKind reports whether err is a CLI error of one of kinds. It never matches an error without a
// kind: the predicates that have a text fallback use classify.
func hasKind(err error, kinds ...string) bool {
	kind, _, ok := classify(err)

	return ok && kind != "" && slices.Contains(kinds, kind)
}

// IsStorageExhausted reports whether err is a refusal for lack of storage: a reservation the
// owner's plan cannot hold (plan_exceeded) or, by prudence, a write refused for lack of storage
// (storage_exceeded). Without a kind, the API's text (`API error 400: {"detail":"Not enough storage
// left on your plan"}`).
func IsStorageExhausted(err error) bool {
	kind, msg, ok := classify(err)
	if kind != "" {
		return kind == KindPlanExceeded || kind == KindStorageExceeded
	}

	return ok && strings.Contains(msg, "not enough storage left")
}

// The text fallbacks, for a CLI that predates the kinds (the pinned image may be one): they follow
// the wording the CLI relays from the API.
var (
	// validationError is the start of the message of a 422, at its beginning or after a prefix of the
	// CLI's ("creating dataroom: API error 422: ..."), not anywhere in the text.
	validationError = regexp.MustCompile(`(^|: )api error 422:`)
	// notFoundError is the start of the message of a 404, anchored the same way: a "not found" or a
	// "404" elsewhere in the text (a 500 relaying a backend's 404, a title) is no missing resource.
	notFoundError = regexp.MustCompile(`(^|: )api error 404:`)
	// minimumCapacity is the API's refusal of a reservation below its minimum.
	minimumCapacity = regexp.MustCompile(`must be at least \d+ bytes`)
)

// IsValidation reports whether err is the API's validation refusing the request: kind invalid (a
// 400 or 422 the CLI does not classify further). Without a kind, any 422.
func IsValidation(err error) bool {
	kind, msg, ok := classify(err)
	if kind != "" {
		return kind == KindInvalid
	}

	return ok && validationError.MatchString(msg)
}

// IsCapacityRefused reports whether err is a reservation below the API's minimum
// (capacity_too_small). Without a kind, a 422 naming the storage_capacity field or the minimum
// ("must be at least 21474836480 bytes").
func IsCapacityRefused(err error) bool {
	kind, msg, ok := classify(err)
	if kind != "" {
		return kind == KindCapacityTooSmall
	}

	return ok && validationError.MatchString(msg) &&
		(strings.Contains(msg, "storage_capacity") || minimumCapacity.MatchString(msg))
}

// IsNotEnlarged reports whether err is the API's refusal to shrink a storage reservation
// (capacity_shrink). Without a kind, its text ("... can only be enlarged").
func IsNotEnlarged(err error) bool {
	kind, msg, ok := classify(err)
	if kind != "" {
		return kind == KindCapacityShrink
	}

	return ok && strings.Contains(msg, "can only be enlarged")
}

// IsNotFound reports whether err is a missing resource, the API's 404 (not_found). Without a kind,
// the 404 the CLI relays (`deleting dataroom: API error 404: {"detail":"Dataroom not found"}`),
// matched at the start of the message or after a prefix of the CLI's (see notFoundError).
func IsNotFound(err error) bool {
	kind, msg, ok := classify(err)
	if kind != "" {
		return kind == KindNotFound || kind == KindGone
	}

	return ok && notFoundError.MatchString(msg)
}

// IsUnauthenticated reports whether the login was refused: no stored token or a rejected refresh
// token (auth_required, exit code 77), or the API answering 401 (unauthenticated). Kind only.
func IsUnauthenticated(err error) bool {
	return hasKind(err, KindAuthRequired, KindUnauthenticated)
}

// IsConfig reports whether the key passphrase is missing or wrong, or the environment file refused
// (config, exit code 78). Kind only.
func IsConfig(err error) bool { return hasKind(err, KindConfig) }

// IsForbidden reports whether the API answered 403 (a viewer, a dataroom over its plan). Kind only.
func IsForbidden(err error) bool { return hasKind(err, KindForbidden) }

// IsRateLimited reports whether the API's rate limit held after the CLI's own retries. Kind only.
func IsRateLimited(err error) bool { return hasKind(err, KindRateLimited) }

// IsCLITooOld reports whether the API refused this CLI as older than the minimum it accepts (426).
// Kind only.
func IsCLITooOld(err error) bool { return hasKind(err, KindCLITooOld) }

// stderrError is the JSON object retyc prints on stderr with --json. An older CLI prints only
// "error"; the other fields are then zero.
type stderrError struct {
	Error  string `json:"error"`
	Kind   string `json:"kind"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// parseStderrError extracts the JSON error object from stderr. The CLI's spinner writes ANSI escape
// sequences to stderr even without a TTY (verified: `\r\x1b[K\x1b[36m⠋...` precedes the JSON on the
// same line), so a plain Unmarshal of the whole buffer fails — locate the JSON object instead of
// assuming stderr is clean. Without one, Error is the cleaned stderr text and the rest is zero.
func parseStderrError(stderr []byte) stderrError {
	// The CLI's contract: the error object is the last line of stderr, after any text warnings.
	lines := bytes.Split(bytes.TrimSpace(stderr), []byte("\n"))
	if last := bytes.TrimSpace(lines[len(lines)-1]); len(last) > 0 && last[0] == '{' {
		var payload stderrError
		if err := json.Unmarshal(last, &payload); err == nil && payload.Error != "" {
			return payload
		}
	}
	// Older CLIs: the spinner's escape sequences may precede the object on its line.
	// Try every '{' from the first one on: the payload's own message may embed JSON (the API
	// error body, e.g. {"detail":"Dataroom not found"}), so the *last* '{' is usually wrong,
	// and the escape noise never contains '{' so the first candidate is normally right.
	for idx := bytes.IndexByte(stderr, '{'); idx >= 0; {
		// Decode stops after the first value: whatever the CLI may write after the object is ignored.
		var payload stderrError
		if err := json.NewDecoder(bytes.NewReader(stderr[idx:])).Decode(&payload); err == nil && payload.Error != "" {
			return payload
		}
		next := bytes.IndexByte(stderr[idx+1:], '{')
		if next < 0 {
			break
		}
		idx += 1 + next
	}

	return stderrError{Error: strings.TrimSpace(stripANSI(string(stderr)))}
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
		payload := parseStderrError(stderr.Bytes())
		if payload.Error == "" {
			payload.Error = err.Error()
		}

		return &Error{
			Args: args, Message: payload.Error, Kind: payload.Kind, Status: payload.Status, Detail: payload.Detail,
		}
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

// CreateDataroom creates a new dataroom titled title (the CSI volume name, always unique). A
// non-nil capacity reserves that many bytes on the owner's plan (--storage-capacity); a non-nil
// versioning sets --versioning=true|false. Nil leaves the CLI's default.
func (c *Client) CreateDataroom(
	ctx context.Context, title string, capacity *int64, versioning *bool,
) (*DataroomCreateResult, error) {
	args := append([]string{"dataroom", "create", "--title", title}, settingArgs(capacity, versioning)...)
	var result DataroomCreateResult
	if err := c.run(ctx, &result, args...); err != nil {
		return nil, err
	}

	return &result, nil
}

// UpdateDataroom changes the storage reservation and/or the versioning of dataroom id through
// `dataroom set`; only the non-nil settings are sent, and with both nil nothing runs. -y is
// mandatory alongside --json: disabling versioning asks for a confirmation the CLI refuses to
// prompt for with --json. The reservation can only be enlarged: the API refuses a shrink
// (capacity_shrink) and accepts the current value without effect.
func (c *Client) UpdateDataroom(ctx context.Context, id string, capacity *int64, versioning *bool) error {
	settings := settingArgs(capacity, versioning)
	if len(settings) == 0 {
		return nil
	}
	args := append([]string{"dataroom", "set", "retyc://" + id}, settings...)

	// The JSON printed on success is not needed: the change either applied or failed.
	return c.run(ctx, nil, append(args, "-y")...)
}

// settingArgs renders the optional dataroom settings shared by `dataroom create` and `dataroom set`.
func settingArgs(capacity *int64, versioning *bool) []string {
	var args []string
	if capacity != nil {
		args = append(args, "--storage-capacity", FormatSize(*capacity))
	}
	if versioning != nil {
		// A boolean flag only takes its value in the --flag=value form.
		args = append(args, "--versioning="+strconv.FormatBool(*versioning))
	}

	return args
}

// FormatSize renders bytes for --storage-capacity in the largest binary unit that divides it
// exactly (the CLI reads binary units: 50G = 50GiB), so the value is never rounded: 10Gi gives
// "10GiB", 1500Mi gives "1500MiB", an odd byte count gives "<n>B". Every suffix emitted (B, KiB,
// MiB, GiB, TiB) is accepted by the CLI's ui.ParseSize, which also takes a bare byte count; it
// refuses zero and negative sizes, so callers must only pass bytes > 0 (0 renders "0B", rejected).
func FormatSize(bytes int64) string {
	units := []struct {
		suffix string
		size   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}
	for _, u := range units {
		if bytes > 0 && bytes%u.size == 0 {
			return strconv.FormatInt(bytes/u.size, 10) + u.suffix
		}
	}

	return strconv.FormatInt(bytes, 10) + "B"
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
	// StorageCapacity is the reserved capacity in bytes; nil when the dataroom is bounded by the
	// owner's plan instead (no reservation).
	StorageCapacity *int64 `json:"storage_capacity,omitempty"`
	// VersioningEnabled is nil when the API predates the setting (versioning on).
	VersioningEnabled *bool `json:"versioning_enabled,omitempty"`
}

// DataroomList is the result of ListDatarooms.
type DataroomList struct {
	Items []Dataroom
}

// dataroomPage mirrors retyc's pagedJSON[api.Dataroom].
type dataroomPage struct {
	Items []Dataroom `json:"items"`
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}

// ListDatarooms returns every dataroom visible to the authenticated identity: `retyc dataroom
// ls` walks all the pages itself and prints them as one list.
func (c *Client) ListDatarooms(ctx context.Context) (*DataroomList, error) {
	var page dataroomPage
	if err := c.run(ctx, &page, "dataroom", "ls"); err != nil {
		return nil, err
	}

	return &DataroomList{Items: page.Items}, nil
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

// WithEnv returns a copy of c whose subprocesses see overrides applied on top of c's environment
// (per-tenant credentials from CSI secrets). c itself is unchanged.
func (c *Client) WithEnv(overrides map[string]string) *Client {
	clone := *c
	clone.Env = identity.MergeEnv(c.Env, overrides)

	return &clone
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
