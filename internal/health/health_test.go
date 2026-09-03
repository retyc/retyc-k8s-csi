package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestHandler_HealthzIsUnconditional(t *testing.T) {
	h := Handler(func(context.Context) error { return errors.New("webdav down") })
	if code, body := get(t, h, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 ok even when not ready", code, body)
	}
}

func TestHandler_ReadyzReportsCheckerError(t *testing.T) {
	h := Handler(func(context.Context) error { return errors.New("webdav down") })
	code, body := get(t, h, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "webdav down") {
		t.Fatalf("/readyz = %d %q, want 503 with the reason", code, body)
	}

	h = Handler(func(context.Context) error { return nil })
	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", code)
	}

	if code, _ := get(t, Handler(nil), "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz with nil checker = %d, want 200", code)
	}
}

func TestCached_RunsOncePerTTL(t *testing.T) {
	calls := 0
	c := Cached(func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("first")
		}

		return nil
	}, time.Hour)

	ctx := context.Background()
	if err := c(ctx); err == nil || err.Error() != "first" {
		t.Fatalf("first call: %v", err)
	}
	if err := c(ctx); err == nil || err.Error() != "first" || calls != 1 {
		t.Fatalf("second call must replay the cached error: err=%v calls=%d", err, calls)
	}
}
