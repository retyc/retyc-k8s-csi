package retycclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Real stderr captured from `retyc --json dataroom rm retyc://0000... -y` on 2026-09-03: the
// spinner's ANSI escapes precede the JSON payload on the same line.
const realStderr = "\r\x1b[K\x1b[36m⠋\x1b[0m\r\x1b[K\x1b[0m" +
	`{"error":"deleting dataroom: API error 404: {\"detail\":\"Dataroom not found\"}"}` + "\n"

func TestParseStderrError_SkipsSpinnerNoise(t *testing.T) {
	got := parseStderrError([]byte(realStderr))
	want := `deleting dataroom: API error 404: {"detail":"Dataroom not found"}`
	if got != want {
		t.Fatalf("parseStderrError:\n got %q\nwant %q", got, want)
	}
}

func TestParseStderrError_NonJSONFallsBackToCleanText(t *testing.T) {
	got := parseStderrError([]byte("\r\x1b[K\x1b[36m⠋\x1b[0mpanic: something broke\n"))
	// Escape sequences and \r are gone; the spinner's braille glyph is plain text and may stay.
	if !strings.Contains(got, "panic: something broke") || strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("got %q", got)
	}
}

func TestIsNotFound(t *testing.T) {
	err := &Error{Args: []string{"dataroom", "rm"}, Message: parseStderrError([]byte(realStderr))}
	if !IsNotFound(err) {
		t.Fatalf("expected IsNotFound for %v", err)
	}
	if IsNotFound(&Error{Message: "API error 500: boom"}) {
		t.Fatal("500 must not be treated as not-found")
	}
	if IsNotFound(errors.New("not found but not a retyc error")) {
		t.Fatal("plain errors must not match")
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
  "dataroom rm")
    printf '\r\033[K{"error":"deleting dataroom: API error 404: {\\"detail\\":\\"Dataroom not found\\"}"}\n' >&2
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

	created, err := c.CreateDataroom(ctx, "pvc-x")
	if err != nil || created.ID != "dr-1" || created.Title != "pvc-x" {
		t.Fatalf("CreateDataroom: %+v, %v", created, err)
	}

	list, err := c.ListDatarooms(ctx)
	if err != nil || len(list.Items) != 1 || list.Complete {
		t.Fatalf("ListDatarooms: %+v, %v (Complete must be false when pages>1)", list, err)
	}

	_, err = c.DeleteDataroom(ctx, "dr-404")
	if !IsNotFound(err) {
		t.Fatalf("DeleteDataroom: expected not-found, got %v", err)
	}
}
