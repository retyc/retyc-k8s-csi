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
	"os/exec"
	"time"

	"k8s.io/klog/v2"
)

// Supervisor keeps `retyc webdav serve` running on Addr:Port, restarting it if it exits.
type Supervisor struct {
	BinPath string
	Env     []string
	Addr    string
	Port    int
}

// BaseURL returns the root URL of the supervised WebDAV server.
func (s *Supervisor) BaseURL() string {
	return fmt.Sprintf("http://%s", net.JoinHostPort(s.Addr, fmt.Sprint(s.Port)))
}

// Run starts the supervised loop and blocks until ctx is cancelled. Call it in its own goroutine.
// A crash is logged and retried with a fixed backoff; ctx cancellation stops the loop and the
// child (via CommandContext).
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
		cmd.Stdout = klogWriter{}
		cmd.Stderr = klogWriter{}

		err := cmd.Run()
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

// klogWriter forwards a child process's stdout/stderr lines into klog, prefixed for provenance.
type klogWriter struct{}

func (klogWriter) Write(p []byte) (int, error) {
	klog.Infof("retyc webdav serve: %s", string(p))

	return len(p), nil
}
