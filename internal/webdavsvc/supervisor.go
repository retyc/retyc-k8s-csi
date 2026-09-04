// Package webdavsvc supervises a single long-lived `retyc webdav serve` child process per node.
// One process exposes every dataroom the node identity can see under /dataroom/<title>; the node
// plugin mounts individual dataroom subpaths via davfs2 against it (see the plan: no --auth,
// since the server and the davfs2 client both run inside the same node-plugin container/netns —
// loopback-only really is loopback-only here, not a wider trust boundary).
package webdavsvc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

// Supervisor keeps `retyc webdav serve` running on Addr:Port, restarting it if it exits.
type Supervisor struct {
	BinPath string
	// Env is the child's complete environment (exec.Cmd semantics: replaces, not appends).
	Env  []string
	Addr string
	Port int

	mu     sync.Mutex
	status Status
}

// Status is a snapshot of the supervised process, surfaced by /readyz and CSI Probe.
type Status struct {
	// Running is true while a child process is alive (it may still be starting up).
	Running bool
	// StartedAt is when the current (or last) child was started.
	StartedAt time.Time
	// ConsecutiveFailures counts child exits since the last time Healthy() saw it serving.
	ConsecutiveFailures int
	// LastExitError is the error of the most recent child exit ("" if none yet).
	LastExitError string
	// LastOutput is the last line the child wrote to stdout/stderr — with a crash-looping
	// `retyc webdav serve` this is the actual reason ("key passphrase check failed: ...").
	LastOutput string
}

// Status returns a copy of the current process state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.status
}

func (s *Supervisor) setRunning(running bool, exitErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running = running
	if running {
		s.status.StartedAt = time.Now()

		return
	}
	s.status.ConsecutiveFailures++
	s.status.LastExitError = ""
	if exitErr != nil {
		s.status.LastExitError = exitErr.Error()
	}
}

func (s *Supervisor) setLastOutput(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastOutput = line
}

// healthyTimeout bounds the HTTP round-trip Healthy makes against the loopback server.
const healthyTimeout = 2 * time.Second

// Healthy reports nil when the supervised server is alive and answers HTTP on BaseURL, or an
// error that says why not (process state, last output, connection error). A success resets
// ConsecutiveFailures.
func (s *Supervisor) Healthy(ctx context.Context) error {
	st := s.Status()
	if !st.Running {
		return fmt.Errorf("retyc webdav serve is not running (%d consecutive exits, last: %s; last output: %q)",
			st.ConsecutiveFailures, st.LastExitError, st.LastOutput)
	}

	ctx, cancel := context.WithTimeout(ctx, healthyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, s.BaseURL()+"/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("retyc webdav serve not answering on %s (started %s ago; last output: %q): %w",
			s.BaseURL(), time.Since(st.StartedAt).Round(time.Second), st.LastOutput, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("retyc webdav serve answered HTTP %d on OPTIONS /", resp.StatusCode)
	}

	s.mu.Lock()
	s.status.ConsecutiveFailures = 0
	s.mu.Unlock()

	return nil
}

// BaseURL returns the root URL of the supervised WebDAV server.
func (s *Supervisor) BaseURL() string {
	return "http://" + net.JoinHostPort(s.Addr, fmt.Sprint(s.Port))
}

// Run starts the supervised loop and blocks until ctx is cancelled. Call it in its own goroutine.
// A crash is logged and retried with a fixed backoff. On ctx cancellation the child gets SIGTERM
// (retyc webdav serve drains in-flight uploads and cleans up orphaned nodes on SIGTERM; the
// default exec.CommandContext behaviour is SIGKILL, which would skip that) and SIGKILL if it's
// still alive after the retyc server's own 15s drain bound.
func (s *Supervisor) Run(ctx context.Context) {
	const restartBackoff = 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		klog.Infof("webdavsvc: starting `retyc webdav serve --addr %s --port %d`", s.Addr, s.Port)
		//nolint:gosec // G204: BinPath is controller-configured, not user input
		cmd := exec.CommandContext(ctx, s.BinPath, "webdav", "serve",
			"--addr", s.Addr, "--port", fmt.Sprint(s.Port))
		cmd.Env = s.Env
		cmd.Stdout = klogWriter{onLine: s.setLastOutput}
		cmd.Stderr = klogWriter{onLine: s.setLastOutput}
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 20 * time.Second

		s.setRunning(true, nil)
		err := cmd.Run()
		s.setRunning(false, err)
		if ctx.Err() != nil {
			return
		}
		klog.Errorf("webdavsvc: retyc webdav serve exited: %v; restarting in %s", err, restartBackoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(restartBackoff):
		}
	}
}

// WaitReady blocks until the server accepts TCP connections, or ctx is done.
func (s *Supervisor) WaitReady(ctx context.Context) error {
	addr := net.JoinHostPort(s.Addr, fmt.Sprint(s.Port))
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()

			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for webdav server on %s: %w", addr, ctx.Err())
		case <-ticker.C:
		}
	}
}

// klogWriter forwards a child process's stdout/stderr into klog, prefixed for provenance, and
// hands the last non-empty line to onLine so the supervisor can report it in Status.
type klogWriter struct {
	onLine func(string)
}

func (w klogWriter) Write(p []byte) (int, error) {
	if msg := strings.TrimRight(string(p), "\n"); msg != "" {
		klog.Infof("retyc webdav serve: %s", msg)
		if w.onLine != nil {
			lines := strings.Split(msg, "\n")
			w.onLine(strings.TrimSpace(lines[len(lines)-1]))
		}
	}

	return len(p), nil
}
