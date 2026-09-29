// Package webdavsvc supervises a single long-lived `retyc webdav serve` child process per node.
// One process exposes every dataroom the node identity can see under /dataroom/<title>; the node
// plugin mounts individual dataroom subpaths via davfs2 against it (no --auth:
// since the server and the davfs2 client both run inside the same node-plugin container/netns —
// loopback-only really is loopback-only here, not a wider trust boundary).
package webdavsvc

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

// Exit codes of `retyc webdav serve` saying a restart with the same environment will fail again
// (sysexits.h, a stable contract of retyc-cli): the login cannot recover without a new token (none
// stored, refresh token expired or revoked), or the key passphrase is missing or wrong. Any other
// code, 1 included, may be transient (Retyc API or identity provider unreachable).
const (
	exitAuthRequired = 77 // EX_NOPERM
	exitConfig       = 78 // EX_CONFIG
)

// Restart policy defaults, used when the matching Supervisor field is zero.
const (
	defaultRestartBackoff    = 5 * time.Second
	defaultMaxRestartBackoff = 5 * time.Minute
	defaultStableAfter       = time.Minute
	defaultMaxFatalExits     = 3
)

// Supervisor keeps `retyc webdav serve` running on Addr:Port, restarting it if it exits.
//
// Restarts back off exponentially, so a server that cannot start (Retyc API down, revoked token)
// does not hammer the API and the identity provider from every node. An exit code saying the
// credentials themselves are unusable (77, 78) is retried MaxFatalExits times, in case the identity
// provider was wrong for a moment, then the supervisor gives up: those credentials will never
// work, and new ones mean a new identity key, hence a new server.
type Supervisor struct {
	BinPath string
	// Env is the child's complete environment (exec.Cmd semantics: replaces, not appends).
	Env  []string
	Addr string
	Port int
	// MetricsPort is the child's second listener (`--metrics-addr`, on Addr), serving /readyz.
	// It binds once the login check and key unlock succeeded; /readyz is then 200 while the
	// WebDAV port serves and 503 as soon as a shutdown starts (signal, expired login). Probing
	// it never calls the Retyc API.
	MetricsPort int

	// RestartBackoff is the delay before the first restart; it doubles with every consecutive
	// failure, up to MaxRestartBackoff. Zero means 5 s and 5 min.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// StableAfter is how long a child must have lived for its exit not to count as a consecutive
	// failure: a server that served for hours and lost its login restarts after RestartBackoff,
	// not after the backoff of its last crash loop. Zero means 1 min.
	StableAfter time.Duration
	// MaxFatalExits is how many consecutive 77/78 exits the supervisor tolerates before giving up.
	// Zero means 3.
	MaxFatalExits int

	mu         sync.Mutex
	status     Status
	fatalExits int // consecutive exits with exitAuthRequired or exitConfig
}

// Status is a snapshot of the supervised process, surfaced by /readyz and CSI Probe.
type Status struct {
	// Running is true while a child process is alive (it may still be starting up).
	Running bool
	// StartedAt is when the current (or last) child was started.
	StartedAt time.Time
	// ConsecutiveFailures counts child exits since the child was last seen serving (Healthy or
	// WaitReady) or last ran for StableAfter; it drives the restart backoff.
	ConsecutiveFailures int
	// LastExitError is the error of the most recent child exit ("" if none yet).
	LastExitError string
	// LastExitCode is the exit code of the most recent child exit: 0 before any exit, after a
	// clean one or when the child could not be started, -1 when a signal killed it.
	LastExitCode int
	// NextRestart is when the next child starts, zero while one runs or after giving up.
	NextRestart time.Time
	// GaveUp is true once the supervisor stopped restarting the child: it exited MaxFatalExits
	// times in a row with a code saying the credentials are unusable. Only new credentials (a new
	// server) or a node plugin restart start it again.
	GaveUp bool
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

func (s *Supervisor) setStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running = true
	s.status.StartedAt = time.Now()
	s.status.NextRestart = time.Time{}
}

// setExited records a child exit and returns how long to wait before the next start, or giveUp.
func (s *Supervisor) setExited(exitErr error) (delay time.Duration, giveUp bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.status
	st.Running = false
	if time.Since(st.StartedAt) >= orDefault(s.StableAfter, defaultStableAfter) {
		st.ConsecutiveFailures = 0
	}
	st.ConsecutiveFailures++
	st.LastExitError, st.LastExitCode = "", 0
	if exitErr != nil {
		st.LastExitError = exitErr.Error()
		var exitErrWithCode *exec.ExitError
		if errors.As(exitErr, &exitErrWithCode) {
			st.LastExitCode = exitErrWithCode.ExitCode()
		}
	}

	if exitMeaning(st.LastExitCode) == "" {
		s.fatalExits = 0
	} else {
		s.fatalExits++
	}
	maxFatal := s.MaxFatalExits
	if maxFatal <= 0 {
		maxFatal = defaultMaxFatalExits
	}
	if s.fatalExits >= maxFatal {
		st.GaveUp = true

		return 0, true
	}

	delay = restartDelay(orDefault(s.RestartBackoff, defaultRestartBackoff),
		orDefault(s.MaxRestartBackoff, defaultMaxRestartBackoff), st.ConsecutiveFailures)
	// Up to 10% of jitter, so the nodes of a cluster that failed together do not retry in step.
	delay += rand.N(delay/10 + 1) //nolint:gosec // G404: jitter, not a secret
	st.NextRestart = time.Now().Add(delay)

	return delay, false
}

// setServing records that the child was seen serving: the next failure starts a fresh backoff.
func (s *Supervisor) setServing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.ConsecutiveFailures = 0
	s.fatalExits = 0
}

// restartDelay is base doubled for every consecutive failure after the first, capped at maxDelay.
func restartDelay(base, maxDelay time.Duration, consecutiveFailures int) time.Duration {
	delay := base
	for i := 1; i < consecutiveFailures && delay < maxDelay; i++ {
		delay *= 2
	}

	return min(delay, maxDelay)
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}

	return d
}

// exitMeaning explains an exit code after which a restart with the same environment fails again,
// or returns "" for any other code.
func exitMeaning(code int) string {
	switch code {
	case exitAuthRequired:
		return "authentication required: RETYC_TOKEN is missing, expired or revoked"
	case exitConfig:
		return "key passphrase missing or wrong"
	}

	return ""
}

// gaveUpError is the error Healthy and WaitReady report once the supervisor gave up.
func gaveUpError(st Status) error {
	return fmt.Errorf("retyc webdav serve gave up after %d consecutive exits, last with code %d (%s); "+
		"fix the credentials: last output: %q",
		st.ConsecutiveFailures, st.LastExitCode, exitMeaning(st.LastExitCode), st.LastOutput)
}

func (s *Supervisor) setLastOutput(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastOutput = line
}

// healthyTimeout bounds the HTTP round-trip Healthy makes against the loopback server.
const healthyTimeout = 2 * time.Second

// Healthy reports nil when the supervised server is alive and its /readyz says it serves, or an
// error that says why not (process state, last output, probe result). A success resets
// ConsecutiveFailures.
func (s *Supervisor) Healthy(ctx context.Context) error {
	st := s.Status()
	if st.GaveUp {
		return gaveUpError(st)
	}
	if !st.Running {
		return fmt.Errorf("retyc webdav serve is not running (%d consecutive exits, last: %s; "+
			"restarting in %s; last output: %q)", st.ConsecutiveFailures, st.LastExitError,
			max(time.Until(st.NextRestart), 0).Round(time.Second), st.LastOutput)
	}

	ctx, cancel := context.WithTimeout(ctx, healthyTimeout)
	defer cancel()
	if err := s.probeReady(ctx); err != nil {
		return fmt.Errorf("retyc webdav serve %w (started %s ago; last output: %q)",
			err, time.Since(st.StartedAt).Round(time.Second), st.LastOutput)
	}

	s.setServing()

	return nil
}

// BaseURL returns the root URL of the supervised WebDAV server.
func (s *Supervisor) BaseURL() string {
	return "http://" + s.hostPort()
}

// hostPort is the host:port the supervised server binds, as `retyc webdav serve --addr` takes it.
func (s *Supervisor) hostPort() string {
	return net.JoinHostPort(s.Addr, fmt.Sprint(s.Port))
}

// metricsHostPort is the host:port of the child's probes listener (`--metrics-addr`).
func (s *Supervisor) metricsHostPort() string {
	return net.JoinHostPort(s.Addr, fmt.Sprint(s.MetricsPort))
}

// probeReady GETs the child's /readyz once. The error reads after "retyc webdav serve".
func (s *Supervisor) probeReady(ctx context.Context) error {
	readyz := "http://" + s.metricsHostPort() + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyz, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("not answering on %s: %w", readyz, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: HTTP %d on %s (starting up or shutting down)", resp.StatusCode, readyz)
	}

	return nil
}

// Run starts the supervised loop and blocks until ctx is cancelled or the supervisor gives up (see
// Supervisor). Call it in its own goroutine. A crash is logged and retried with an exponential
// backoff. On ctx cancellation the child gets SIGTERM
// (retyc webdav serve drains in-flight uploads and cleans up orphaned nodes on SIGTERM; the
// default exec.CommandContext behaviour is SIGKILL, which would skip that) and SIGKILL if it's
// still alive after the retyc server's own 15s drain bound.
func (s *Supervisor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// The node plugin exposes its own Go runtime metrics; the children's would collide with them.
		// The tenant label is added when the pool merges the children's metrics: it follows the
		// volumes staged through the server, which change during its life.
		args := []string{
			"webdav", "serve", "--addr", s.hostPort(),
			"--metrics-addr", s.metricsHostPort(), "--metrics-runtime=false",
		}
		klog.Infof("webdavsvc: starting `retyc %s`", strings.Join(args, " "))
		//nolint:gosec // G204: BinPath is controller-configured, not user input
		cmd := exec.CommandContext(ctx, s.BinPath, args...)
		cmd.Env = s.Env
		cmd.Stdout = klogWriter{onLine: s.setLastOutput}
		cmd.Stderr = klogWriter{onLine: s.setLastOutput}
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 20 * time.Second

		s.setStarted()
		err := cmd.Run()
		delay, giveUp := s.setExited(err)
		if ctx.Err() != nil {
			return
		}
		if giveUp {
			klog.Errorf("webdavsvc: retyc webdav serve exited: %v; giving up: %v", err, gaveUpError(s.Status()))

			return
		}
		klog.Errorf("webdavsvc: retyc webdav serve exited: %v; restarting in %s", err, delay.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// WaitReady blocks until the server's /readyz reports it serves WebDAV, ctx is done or the
// supervisor gave up.
func (s *Supervisor) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		// A supervisor that gave up never brings the server back: fail now, not at ctx's deadline.
		if st := s.Status(); st.GaveUp {
			return gaveUpError(st)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := s.probeReady(attemptCtx)
		cancel()
		if err == nil {
			s.setServing()

			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for retyc webdav serve on %s: %w (last probe: %v; last output: %q)",
				s.hostPort(), ctx.Err(), err, s.Status().LastOutput)
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
