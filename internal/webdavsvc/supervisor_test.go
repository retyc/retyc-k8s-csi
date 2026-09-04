package webdavsvc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	path := t.TempDir() + "/retyc"
	//nolint:gosec // G306: the fixture is a script and must be executable
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisor_CrashLoopIsReportedWithLastOutput(t *testing.T) {
	s := &Supervisor{
		BinPath: fakeBinary(t, `echo "key passphrase check failed: wrong key passphrase" >&2; exit 1`),
		Addr:    "127.0.0.1", Port: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "first exit", func() bool { return s.Status().ConsecutiveFailures >= 1 })
	st := s.Status()
	if st.Running || !strings.Contains(st.LastExitError, "exit status 1") {
		t.Fatalf("status after crash: %+v", st)
	}
	if !strings.Contains(st.LastOutput, "wrong key passphrase") {
		t.Fatalf("LastOutput must carry the child's last line, got %q", st.LastOutput)
	}

	err := s.Healthy(ctx)
	if err == nil || !strings.Contains(err.Error(), "not running") ||
		!strings.Contains(err.Error(), "wrong key passphrase") {
		t.Fatalf("Healthy while crash-looping: %v", err)
	}
}

func TestSupervisor_HealthyProbesTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			t.Errorf("expected OPTIONS, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	// A child that stays alive stands in for a serving `retyc webdav serve`; the HTTP side is
	// the httptest server above.
	s := &Supervisor{BinPath: fakeBinary(t, "sleep 30"), Addr: host, Port: port}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "child running", func() bool { return s.Status().Running })

	if err := s.Healthy(ctx); err != nil {
		t.Fatalf("Healthy against a serving backend: %v", err)
	}

	srv.Close()
	err := s.Healthy(ctx)
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("Healthy with the HTTP side gone: %v", err)
	}
}
