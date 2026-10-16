package retycclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Real stderr captured from `retyc --json dataroom rm retyc://0000... -y` on 2026-09-03: the
// spinner's ANSI escapes precede the JSON payload on the same line.
const realStderr = "\r\x1b[K\x1b[36m⠋\x1b[0m\r\x1b[K\x1b[0m" +
	`{"error":"deleting dataroom: API error 404: {\"detail\":\"Dataroom not found\"}"}` + "\n"

func TestParseStderrError_SkipsSpinnerNoise(t *testing.T) {
	got := parseStderrError([]byte(realStderr)).Error
	want := `deleting dataroom: API error 404: {"detail":"Dataroom not found"}`
	if got != want {
		t.Fatalf("parseStderrError:\n got %q\nwant %q", got, want)
	}
}

func TestParseStderrError_NonJSONFallsBackToCleanText(t *testing.T) {
	got := parseStderrError([]byte("\r\x1b[K\x1b[36m⠋\x1b[0mpanic: something broke\n")).Error
	// Escape sequences and \r are gone; the spinner's braille glyph is plain text and may stay.
	if !strings.Contains(got, "panic: something broke") || strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("got %q", got)
	}
}

func TestIsNotFound(t *testing.T) {
	err := &Error{Args: []string{"dataroom", "rm"}, Message: parseStderrError([]byte(realStderr)).Error}
	if !IsNotFound(err) {
		t.Fatalf("expected IsNotFound for %v", err)
	}
	if IsNotFound(&Error{Message: "API error 500: boom"}) {
		t.Fatal("500 must not be treated as not-found")
	}
	if IsNotFound(errors.New("API error 404: not a retyc error")) {
		t.Fatal("plain errors must not match")
	}
	for _, msg := range []string{
		`API error 404: {"detail":"Dataroom not found"}`,
		`updating dataroom: API error 404: {"detail":"Dataroom not found"}`,
	} {
		if !IsNotFound(&Error{Message: msg}) {
			t.Errorf("IsNotFound(%q) = false, want true", msg)
		}
	}
	for _, msg := range []string{
		// "not found" or "404" elsewhere in the text is no missing resource.
		`API error 500: {"detail":"key not found in cache"}`,
		`API error 500: upstream said api error 404: boom`,
		`API error 4040: boom`,
		`API error 422: {"detail":"title 'api error 404: x' is not allowed"}`,
		`identity provider not found`,
	} {
		if IsNotFound(&Error{Message: msg}) {
			t.Errorf("IsNotFound(%q) = true, want false", msg)
		}
	}
}

func TestAPIRefusals(t *testing.T) {
	exhausted := &Error{Message: `API error 400: {"detail":"Not enough storage left on your plan"}`}
	tooSmall := &Error{Message: `API error 422: {"detail":"storage_capacity must be at least 21474836480 bytes"}`}
	badTitle := &Error{Message: `API error 422: {"detail":[{"loc":["body","title"],"msg":"field required"}]}`}
	for _, tc := range []struct {
		err                         error
		exhausted, validation, size bool
	}{
		{err: exhausted, exhausted: true},
		{err: tooSmall, validation: true, size: true},
		{err: badTitle, validation: true},
		{err: &Error{Message: "API error 500: boom"}},
		{err: errors.New("API error 422: not a retyc error")},
		// Prefixed by the CLI: still a 422.
		{err: &Error{Message: `updating dataroom: API error 422: {"detail":"storage_capacity is too large"}`},
			validation: true, size: true},
		// "422" elsewhere in the text is no validation error.
		{err: &Error{Message: `API error 500: upstream said api error 422: boom`}},
		{err: &Error{Message: `API error 4220: boom`}},
		// A 422 that mentions a capacity, but not the reservation.
		{err: &Error{Message: `API error 422: {"detail":"title exceeds the capacity of the field"}`}, validation: true},
	} {
		if IsStorageExhausted(tc.err) != tc.exhausted || IsValidation(tc.err) != tc.validation ||
			IsCapacityRefused(tc.err) != tc.size {
			t.Errorf("%v: exhausted %t, validation %t, capacity %t; want %t, %t, %t", tc.err,
				IsStorageExhausted(tc.err), IsValidation(tc.err), IsCapacityRefused(tc.err),
				tc.exhausted, tc.validation, tc.size)
		}
	}
}

// Stderr of a retyc-cli with structured errors (17e7230 on): kind, status and detail beside the
// message, after the spinner's escapes.
const structuredStderr = "\r\x1b[K\x1b[36m⠋\x1b[0m\r\x1b[K" +
	`{"error":"updating dataroom: API error 400: {\"detail\":\"A reservation can only be enlarged\"}",` +
	`"kind":"capacity_shrink","status":400,"detail":"A reservation can only be enlarged"}` + "\n"

func TestParseStderrError_Structured(t *testing.T) {
	got := parseStderrError([]byte(structuredStderr))
	want := stderrError{
		Error:  `updating dataroom: API error 400: {"detail":"A reservation can only be enlarged"}`,
		Kind:   KindCapacityShrink,
		Status: 400,
		Detail: "A reservation can only be enlarged",
	}
	if got != want {
		t.Fatalf("parseStderrError:\n got %+v\nwant %+v", got, want)
	}
	// An older CLI: the message only.
	if got := parseStderrError([]byte(realStderr)); got.Kind != "" || got.Status != 0 || got.Detail != "" {
		t.Fatalf("old CLI stderr: %+v", got)
	}
}

// TestKinds checks that the predicates branch on the kind when there is one, whatever the text says,
// and on the text only without one.
func TestKinds(t *testing.T) {
	kinded := func(kind, msg string) error { return &Error{Kind: kind, Message: msg} }
	type preds struct{ notFound, exhausted, capacity, shrink, validation, auth, config, rate, old, forbidden bool }
	check := func(err error) preds {
		return preds{
			IsNotFound(err), IsStorageExhausted(err), IsCapacityRefused(err), IsNotEnlarged(err), IsValidation(err),
			IsUnauthenticated(err), IsConfig(err), IsRateLimited(err), IsCLITooOld(err), IsForbidden(err),
		}
	}
	for _, tc := range []struct {
		err  error
		want preds
	}{
		{kinded(KindNotFound, "deleting dataroom: API error 404: x"), preds{notFound: true}},
		{kinded(KindPlanExceeded, "API error 400: x"), preds{exhausted: true}},
		{kinded(KindStorageExceeded, "uploading: x"), preds{exhausted: true}},
		{kinded(KindCapacityTooSmall, "API error 422: x"), preds{capacity: true}},
		{kinded(KindCapacityShrink, "API error 400: x"), preds{shrink: true}},
		{kinded(KindInvalid, "API error 422: x"), preds{validation: true}},
		{kinded(KindAuthRequired, "not logged in"), preds{auth: true}},
		{kinded(KindUnauthenticated, "API error 401: x"), preds{auth: true}},
		{kinded(KindConfig, "key passphrase"), preds{config: true}},
		{kinded(KindRateLimited, "API error 429: x"), preds{rate: true}},
		{kinded(KindCLITooOld, "API error 426: x"), preds{old: true}},
		{kinded(KindForbidden, "API error 403: x"), preds{forbidden: true}},
		// The kind wins over a text that the fallback would match.
		{kinded("api_error", `API error 404: {"detail":"Not enough storage left, can only be enlarged"}`),
			preds{}},
		{kinded(KindInvalid, `API error 422: storage_capacity must be at least 21474836480 bytes`),
			preds{validation: true}},
		// An unknown kind reads as a plain error.
		{kinded("brand_new_kind", "API error 404: x"), preds{}},
		// Without a kind (an older CLI), the text.
		{kinded("", `updating dataroom: API error 404: {"detail":"Dataroom not found"}`), preds{notFound: true}},
		{kinded("", `API error 400: a storage reservation can only be enlarged`), preds{shrink: true}},
		{kinded("", `API error 401: {"detail":"Not authenticated"}`), preds{}},
		{errors.New("API error 404: not a retyc error"), preds{}},
	} {
		if got := check(tc.err); got != tc.want {
			t.Errorf("%+v: got %+v, want %+v", tc.err, got, tc.want)
		}
	}
}

// TestRun_FakeBinary drives run() through a shell stand-in for retyc so the exec/decode path is
// covered without network or credentials.
func TestRun_FakeBinary(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	fake := dir + "/retyc"
	script := `#!/bin/sh
# $1 is always --json
case "$2 $3" in
  "dataroom create") printf '{"id":"dr-1","title":"%s"}\n' "$5" ;;
  "dataroom ls")     printf '{"items":[{"id":"dr-1","title":"a"}],"total":30,"page":1,"pages":2}\n' ;;
  "auth status")
    if [ "$RETYC_TOKEN" = "good" ]; then printf '{"authenticated":true,"offline":true}\n'
    else printf '{"authenticated":false,"reason":"no_token"}\n'; fi ;;
  "dataroom rm")
    printf '\r\033[K{"error":"deleting dataroom: API error 404: {\\"detail\\":\\"Dataroom not found\\"}"}\n' >&2
    exit 1 ;;
  "dataroom set")
    printf '{"error":"API error 429: slow down","kind":"rate_limited","status":429}\n' >&2
    exit 1 ;;
  *) echo "unexpected: $*" >&2; exit 2 ;;
esac
`
	//nolint:gosec // G306: the fixture is a script and must be executable
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(fake, nil)
	ctx := context.Background()

	created, err := c.CreateDataroom(ctx, "pvc-x", nil, nil)
	if err != nil || created.ID != "dr-1" || created.Title != "pvc-x" {
		t.Fatalf("CreateDataroom: %+v, %v", created, err)
	}

	list, err := c.ListDatarooms(ctx)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("ListDatarooms: %+v, %v", list, err)
	}

	if err := New(fake, []string{"RETYC_TOKEN=good"}).AuthStatusCheck(ctx); err != nil {
		t.Fatalf("AuthStatusCheck authenticated: %v", err)
	}
	err = New(fake, []string{"RETYC_TOKEN=bad"}).AuthStatusCheck(ctx)
	if err == nil || !strings.Contains(err.Error(), "no_token") {
		t.Fatalf("AuthStatusCheck unauthenticated must fail with the reason, got %v", err)
	}
	bad := New(fake, []string{"RETYC_TOKEN=bad"})
	if err := bad.WithEnv(map[string]string{"RETYC_TOKEN": "good"}).AuthStatusCheck(ctx); err != nil {
		t.Fatalf("WithEnv must override the inherited token: %v", err)
	}
	if err := bad.AuthStatusCheck(ctx); err == nil {
		t.Fatal("WithEnv must not modify the original client")
	}

	_, err = c.DeleteDataroom(ctx, "dr-404")
	if !IsNotFound(err) {
		t.Fatalf("DeleteDataroom: expected not-found, got %v", err)
	}

	capacity := int64(20 << 30)
	err = c.UpdateDataroom(ctx, "dr-1", &capacity, nil)
	var cliErr *Error
	if !errors.As(err, &cliErr) || cliErr.Kind != KindRateLimited || cliErr.Status != 429 || !IsRateLimited(err) ||
		cliErr.Message != "API error 429: slow down" {
		t.Fatalf("UpdateDataroom: want a rate_limited *Error, got %#v", err)
	}
}

// argsRecorder writes a shell stand-in for retyc that appends its arguments to a log file, then
// prints output on stdout (code 0) or on stderr (any other code) and exits with code. It returns
// the client and a function reading the logged command lines.
func argsRecorder(t *testing.T, output string, code int) (*Client, func() []string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	log := dir + "/args"
	if err := os.WriteFile(dir+"/output", []byte(output+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	if code == 0 {
		script += "cat " + dir + "/output\n"
	} else {
		script += "cat " + dir + "/output >&2\nexit " + strconv.Itoa(code) + "\n"
	}
	//nolint:gosec // G306: the fixture is a script and must be executable
	if err := os.WriteFile(dir+"/retyc", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return New(dir+"/retyc", nil), func() []string {
		data, err := os.ReadFile(log) //nolint:gosec // G304: test fixture path
		if err != nil {
			return nil
		}

		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestCreateDataroom_SendsCapacityAndVersioning(t *testing.T) {
	c, calls := argsRecorder(t, `{"id":"dr-1","title":"pvc-x"}`, 0)
	capacity, versioning := int64(10<<30), false
	if _, err := c.CreateDataroom(context.Background(), "pvc-x", &capacity, &versioning); err != nil {
		t.Fatal(err)
	}
	want := "--json dataroom create --title pvc-x --storage-capacity 10GiB --versioning=false"
	if got := calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestUpdateDataroom(t *testing.T) {
	c, calls := argsRecorder(t, `{"id":"dr-1"}`, 0)
	ctx := context.Background()
	capacity, versioning := int64(1500<<20), true
	if err := c.UpdateDataroom(ctx, "dr-1", &capacity, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateDataroom(ctx, "dr-1", nil, &versioning); err != nil {
		t.Fatal(err)
	}
	// Nothing to change: the CLI is not run at all.
	if err := c.UpdateDataroom(ctx, "dr-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--json dataroom set retyc://dr-1 --storage-capacity 1500MiB -y",
		"--json dataroom set retyc://dr-1 --versioning=true -y",
	}
	if got := calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestUpdateDataroom_CLIError(t *testing.T) {
	c, _ := argsRecorder(t, `{"error":"API error 404: {\"detail\":\"Dataroom not found\"}"}`, 1)
	versioning := false
	err := c.UpdateDataroom(context.Background(), "dr-404", nil, &versioning)
	var cliErr *Error
	if !errors.As(err, &cliErr) || !IsNotFound(err) {
		t.Fatalf("UpdateDataroom: want a not-found *Error, got %v", err)
	}
}

func TestFormatSize(t *testing.T) {
	for bytes, want := range map[int64]string{
		1 << 40:       "1TiB",
		10 << 30:      "10GiB",
		1536 << 20:    "1536MiB",
		4096:          "4KiB",
		512:           "512B",
		1 << 50:       "1024TiB",
		1_000_000_000: "1000000000B",
		0:             "0B",
	} {
		if got := FormatSize(bytes); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

// A line after the JSON object does not lose its kind; a 410 reads as not found.
func TestParseStderrErrorTrailingOutput(t *testing.T) {
	got := parseStderrError([]byte(`{"error":"gone","kind":"gone","status":410}` + "\nbye\n"))
	if got.Kind != KindGone || got.Status != 410 {
		t.Fatalf("got %+v", got)
	}
	if !IsNotFound(&Error{Message: got.Error, Kind: got.Kind}) {
		t.Fatal("a 410 is not read as not found")
	}
}

// Text warnings before the object, which is the last line of stderr.
func TestParseStderrErrorLastLine(t *testing.T) {
	stderr := "warning: RETYC_TOKEN is expired or revoked, falling back to stored disk token\n" +
		`{"error":"not authenticated","kind":"auth_required"}` + "\n"
	if got := parseStderrError([]byte(stderr)); got.Kind != KindAuthRequired {
		t.Fatalf("got %+v", got)
	}
}
