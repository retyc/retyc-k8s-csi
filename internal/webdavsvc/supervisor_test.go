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

func TestRestartDelay(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute},
		{1000, 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := restartDelay(5*time.Second, 5*time.Minute, tt.failures); got != tt.want {
			t.Errorf("restartDelay after %d failures = %s, want %s", tt.failures, got, tt.want)
		}
	}
}

// countingBinary is a fake `retyc webdav serve` that appends a line to a file on every start, then
// runs script. starts reads the count back.
func countingBinary(t *testing.T, script string) (bin string, starts func() int) {
	t.Helper()
	countFile := t.TempDir() + "/starts"
	bin = fakeBinary(t, `echo start >> `+countFile+"\n"+`n=$(wc -l < `+countFile+")\n"+script)

	return bin, func() int {
		b, _ := os.ReadFile(countFile) //nolint:gosec // G304: a t.TempDir() fixture

		return strings.Count(string(b), "start")
	}
}

// A revoked token (exit 77) is retried MaxFatalExits times, then the supervisor stops restarting
// and reports why, without WaitReady waiting for its deadline.
func TestSupervisor_GivesUpOnFatalExitCode(t *testing.T) {
	bin, starts := countingBinary(t, `echo "not authenticated, run retyc auth login: no stored token" >&2; exit 77`)
	s := &Supervisor{BinPath: bin, Addr: "127.0.0.1", Port: 1, MetricsPort: 1, RestartBackoff: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return once it gave up")
	}
	st := s.Status()
	if !st.GaveUp || st.LastExitCode != exitAuthRequired || starts() != defaultMaxFatalExits {
		t.Fatalf("after giving up: status %+v, %d starts", st, starts())
	}

	err := s.Healthy(ctx)
	if err == nil || !strings.Contains(err.Error(), "gave up") || !strings.Contains(err.Error(), "code 77") ||
		!strings.Contains(err.Error(), "no stored token") {
		t.Fatalf("Healthy after giving up: %v", err)
	}
	start := time.Now()
	wait, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWait()
	if err := s.WaitReady(wait); err == nil || !strings.Contains(err.Error(), "gave up") ||
		time.Since(start) > time.Second {
		t.Fatalf("WaitReady after giving up must fail at once: %v after %s", err, time.Since(start))
	}
}

// Exit 1 may be transient (API unreachable): never give up, only back off.
func TestSupervisor_KeepsRestartingOnGenericExit(t *testing.T) {
	bin, starts := countingBinary(t, `exit 1`)
	s := &Supervisor{BinPath: bin, Addr: "127.0.0.1", Port: 1, RestartBackoff: time.Millisecond,
		MaxRestartBackoff: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "restarts past the fatal limit", func() bool { return starts() > 2*defaultMaxFatalExits })
	if st := s.Status(); st.GaveUp || st.LastExitCode != 1 {
		t.Fatalf("status after generic exits: %+v", st)
	}
}

// Only consecutive fatal exits count: a transient failure in between starts the count over.
func TestSupervisor_FatalExitsMustBeConsecutive(t *testing.T) {
	// Starts 1, 2: 77; start 3: 1; then 77 for good.
	bin, starts := countingBinary(t, `[ "$n" -eq 3 ] && exit 1; exit 77`)
	s := &Supervisor{BinPath: bin, Addr: "127.0.0.1", Port: 1, RestartBackoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must give up")
	}
	if got := starts(); got != 3+defaultMaxFatalExits {
		t.Fatalf("gave up after %d starts, want %d", got, 3+defaultMaxFatalExits)
	}
}

// A child that lived past StableAfter restarts after the base backoff, whatever came before.
func TestSupervisor_StableRunResetsBackoff(t *testing.T) {
	bin, starts := countingBinary(t, `sleep 0.2; exit 1`)
	s := &Supervisor{BinPath: bin, Addr: "127.0.0.1", Port: 1, RestartBackoff: time.Millisecond,
		StableAfter: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "a few restarts", func() bool { return starts() >= 3 })
	if st := s.Status(); st.ConsecutiveFailures > 1 {
		t.Fatalf("exits after a stable run must not pile up: %+v", st)
	}
}

// Cancelling ctx during a long backoff stops Run at once.
func TestSupervisor_CancelDuringBackoff(t *testing.T) {
	s := &Supervisor{BinPath: fakeBinary(t, `exit 1`), Addr: "127.0.0.1", Port: 1, RestartBackoff: time.Hour,
		MaxRestartBackoff: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, "first exit", func() bool { return s.Status().ConsecutiveFailures >= 1 })
	if err := s.Healthy(ctx); err == nil || !strings.Contains(err.Error(), "restarting in 1h") {
		t.Fatalf("Healthy during backoff must say when the next start is: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return when ctx is cancelled during a backoff")
	}
}
