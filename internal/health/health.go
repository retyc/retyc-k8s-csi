// Package health serves the HTTP liveness/readiness endpoints of the driver.
//
// The two endpoints deliberately mean different things. /healthz only says "the process is up
// and serving"; it backs the kubelet livenessProbe, and a failure there restarts the container.
// On the node plugin a restart kills every davfs2 mount on the node (the FUSE daemons live in
// the container), so liveness must never trip on a transient condition. /readyz carries the real
// diagnosis (local webdav server answering, Retyc credentials valid) and backs the
// readinessProbe, which only flips the pod to NotReady and records why.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Checker reports why the plugin is not ready, or nil when it is.
type Checker func(ctx context.Context) error

// checkTimeout bounds a single /readyz evaluation so a hung dependency cannot pin the probe.
const checkTimeout = 5 * time.Second

// Handler returns the HTTP mux serving /healthz and /readyz. ready may be nil, in which case
// /readyz always succeeds (there is nothing mode-specific to check).
func Handler(ready Checker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
			defer cancel()
			if err := ready(ctx); err != nil {
				// kubelet's event only carries the status code; the reason has to be somewhere
				// `kubectl logs` shows it. One line per failed probe, i.e. only while broken.
				klog.Warningf("/readyz: not ready: %v", err)
				http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)

				return
			}
		}
		fmt.Fprintln(w, "ok")
	})

	return mux
}

// ListenAndServe serves handler on addr until ctx is done, then shuts down gracefully.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	klog.Infof("health endpoints listening on %s (/healthz, /readyz)", listener.Addr())
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// Cached wraps check so it runs at most once per ttl; callers in between get the last result.
// Meant for checks that cost an API round-trip (e.g. `retyc auth status`), which must not run
// on every kubelet probe.
func Cached(check Checker, ttl time.Duration) Checker {
	var (
		mu      sync.Mutex
		last    error
		checked time.Time
	)

	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !checked.IsZero() && time.Since(checked) < ttl {
			return last
		}
		last = check(ctx)
		checked = time.Now()

		return last
	}
}
