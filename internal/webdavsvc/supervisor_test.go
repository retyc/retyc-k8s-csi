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
	"sync/atomic"
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

// retyc-cli v1.3.0 dropped --port: --addr (and --metrics-addr) take host:port, and an unknown flag
// makes the child crash-loop on startup.
func TestSupervisor_PassesAddrAsHostPort(t *testing.T) {
	argsFile := t.TempDir() + "/args"
	s := &Supervisor{
		BinPath: fakeBinary(t, `echo "$@" > `+argsFile+`; exec sleep 30`),
		Addr:    "127.0.0.1", Port: 8888, MetricsPort: 8889,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	var got string
	waitFor(t, "child args", func() bool {
		b, err := os.ReadFile(argsFile) //nolint:gosec // G304: a t.TempDir() fixture
		got = strings.TrimSpace(string(b))

		return err == nil && got != ""
	})
	if want := "webdav serve --addr 127.0.0.1:8888 --metrics-addr 127.0.0.1:8889 --metrics-runtime=false"; got != want {
		t.Fatalf("child args = %q, want %q", got, want)
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

// readyzServer stands in for the probes listener of `retyc webdav serve --metrics-addr`, answering
// GET /readyz with *status. It returns the host and port to set on a Supervisor.
func readyzServer(t *testing.T, status *atomic.Int32) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/readyz" {
			t.Errorf("expected GET /readyz, got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	return srv, host, port
}

func TestSupervisor_HealthyProbesReadyz(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, host, port := readyzServer(t, &status)

	// A child that stays alive stands in for a serving `retyc webdav serve`; the HTTP side is
	// the httptest server above.
	s := &Supervisor{BinPath: fakeBinary(t, "exec sleep 30"), Addr: host, Port: 1, MetricsPort: port}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "child running", func() bool { return s.Status().Running })

	if err := s.Healthy(ctx); err != nil {
		t.Fatalf("Healthy against a ready server: %v", err)
	}

	status.Store(http.StatusServiceUnavailable) // starting up, or shutting down on an expired login
	err := s.Healthy(ctx)
	if err == nil || !strings.Contains(err.Error(), "not ready: HTTP 503") {
		t.Fatalf("Healthy with /readyz at 503: %v", err)
	}

	srv.Close()
	err = s.Healthy(ctx)
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("Healthy with the HTTP side gone: %v", err)
	}
}

func TestSupervisor_WaitReadyWaitsForReadyz(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	_, host, port := readyzServer(t, &status)
	s := &Supervisor{Addr: host, Port: 1, MetricsPort: port}

	// The probes listener is up but the WebDAV port is not bound yet: still waiting.
	short, cancelShort := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelShort()
	if err := s.WaitReady(short); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("WaitReady while /readyz is 503: %v", err)
	}

	time.AfterFunc(300*time.Millisecond, func() { status.Store(http.StatusOK) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady once /readyz turns 200: %v", err)
	}
}
