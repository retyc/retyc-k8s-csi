package webdavsvc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// childMetrics is what a `retyc webdav serve --metrics-runtime=false` exposes.
const childMetrics = `# HELP retyc_cli_webdav_requests_total WebDAV requests served.
# TYPE retyc_cli_webdav_requests_total counter
retyc_cli_webdav_requests_total{method="GET",status="200"} 3
# HELP retyc_cli_webdav_request_duration_seconds WebDAV request latency.
# TYPE retyc_cli_webdav_request_duration_seconds histogram
retyc_cli_webdav_request_duration_seconds_bucket{method="GET",le="1"} 3
retyc_cli_webdav_request_duration_seconds_bucket{method="GET",le="+Inf"} 3
retyc_cli_webdav_request_duration_seconds_sum{method="GET"} 0.5
retyc_cli_webdav_request_duration_seconds_count{method="GET"} 3
`

// metricsServer stands in for the probes listener of a `retyc webdav serve`, serving handler on
// /metrics, and returns a running Supervisor pointing at it.
func metricsServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Supervisor) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	sup := &Supervisor{Addr: host, MetricsPort: port}
	sup.status.Running = true

	return srv, sup
}

func serving(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)

			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func family(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range families {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("family %s missing from %d families", name, len(families))

	return nil
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}

	return "<absent>"
}

// upByIdentity maps retyc_csi_webdav_server_up to identity -> "tenant=value".
func upByIdentity(t *testing.T, families []*dto.MetricFamily) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range family(t, families, upMetric).GetMetric() {
		out[label(m, identityLabel)] = label(m, tenantLabel) + "=" + strconv.FormatFloat(m.GetGauge().GetValue(), 'f', -1, 64)
	}

	return out
}

func TestGatherServers_MergesFamiliesAndReportsUp(t *testing.T) {
	_, a := metricsServer(t, serving(childMetrics))
	_, b := metricsServer(t, serving(childMetrics))
	gone, c := metricsServer(t, serving(childMetrics))
	gone.Close()
	stopped := &Supervisor{Addr: "127.0.0.1", MetricsPort: 1} // not running: never scraped

	families := gatherServers(context.Background(), []scrapeTarget{
		{identity: "aaaa", tenant: defaultTenant, sup: a},
		{identity: "bbbb", tenant: "team-a", sup: b},
		{identity: "cccc", tenant: "team-a", sup: c}, // two tenant Secrets in one namespace
		{identity: "dddd", tenant: "", sup: stopped},
	})

	requests := family(t, families, "retyc_cli_webdav_requests_total")
	got := map[string]string{}
	for _, m := range requests.GetMetric() {
		got[label(m, identityLabel)] = label(m, tenantLabel)
	}
	if len(got) != 2 || got["aaaa"] != defaultTenant || got["bbbb"] != "team-a" {
		t.Fatalf("requests_total must carry one series per answering server, labeled: %v", got)
	}
	histogram := family(t, families, "retyc_cli_webdav_request_duration_seconds")
	if n := len(histogram.GetMetric()); n != 2 {
		t.Fatalf("histogram must carry one series per answering server, got %d", n)
	}
	var names []string
	for _, l := range histogram.GetMetric()[0].GetLabel() {
		names = append(names, l.GetName())
	}
	if strings.Join(names, ",") != "identity,method,tenant" {
		t.Fatalf("labels must be sorted by name, got %v", names)
	}
	want := map[string]string{"aaaa": "_default=1", "bbbb": "team-a=1", "cccc": "team-a=0", "dddd": "=0"}
	if up := upByIdentity(t, families); len(up) != len(want) ||
		up["aaaa"] != want["aaaa"] || up["bbbb"] != want["bbbb"] || up["cccc"] != want["cccc"] || up["dddd"] != want["dddd"] {
		t.Fatalf("%s = %v, want %v", upMetric, up, want)
	}
}

func TestGatherServers_FailedScrapesAreDown(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	_, hung := metricsServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-block:
		}
	})
	_, broken := metricsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, garbage := metricsServer(t, serving("this is { not the exposition format\n"))

	// The caller's deadline bounds a server that never answers, like scrapeTimeout does.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	families := gatherServers(ctx, []scrapeTarget{
		{identity: "hung", sup: hung}, {identity: "broken", sup: broken}, {identity: "garbage", sup: garbage},
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a hung server must not hold the scrape past its deadline, took %s", elapsed)
	}
	if len(families) != 1 {
		t.Fatalf("only %s must be served, got %d families", upMetric, len(families))
	}
	for identity, up := range upByIdentity(t, families) {
		if up != "=0" {
			t.Errorf("identity %s: up = %s, want 0", identity, up)
		}
	}
}

func TestGatherServers_ReservedLabelsAreTheTargets(t *testing.T) {
	_, a := metricsServer(t, serving(`# TYPE retyc_cli_x counter
retyc_cli_x{identity="spoofed",tenant="spoofed",zone="z1"} 1
`))

	families := gatherServers(context.Background(), []scrapeTarget{{identity: "aaaa", tenant: "team-a", sup: a}})

	m := family(t, families, "retyc_cli_x").GetMetric()[0]
	var pairs []string
	for _, l := range m.GetLabel() {
		pairs = append(pairs, l.GetName()+"="+l.GetValue())
	}
	if got := strings.Join(pairs, ","); got != "identity=aaaa,tenant=team-a,zone=z1" {
		t.Fatalf("labels = %s, want the target's identity and tenant, sorted, other labels kept", got)
	}
}

func TestGatherServers_TypeMismatchKeepsOneType(t *testing.T) {
	_, a := metricsServer(t, serving("# TYPE retyc_cli_x counter\nretyc_cli_x 1\n"))
	_, b := metricsServer(t, serving("# TYPE retyc_cli_x gauge\nretyc_cli_x 2\n"))

	families := gatherServers(context.Background(), []scrapeTarget{
		{identity: "aaaa", sup: a}, {identity: "bbbb", sup: b},
	})

	x := family(t, families, "retyc_cli_x")
	if len(x.GetMetric()) != 1 {
		t.Fatalf("the series of the other type must be dropped, got %v", x)
	}
	wantIdentity := map[dto.MetricType]string{dto.MetricType_COUNTER: "aaaa", dto.MetricType_GAUGE: "bbbb"}[x.GetType()]
	if got := label(x.GetMetric()[0], identityLabel); got != wantIdentity {
		t.Fatalf("a %s family kept the series of identity %s", x.GetType(), got)
	}
}

func TestPool_Targets(t *testing.T) {
	sup := func() *Supervisor { return &Supervisor{} }
	p := &Pool{
		servers: map[string]*server{
			"def0": {key: "def0", sup: sup(), pinned: true},
			"aaaa": {key: "aaaa", sup: sup()},
			"bbbb": {key: "bbbb", sup: sup()},
			"cccc": {key: "cccc", sup: sup()},
		},
		byPath: map[string]string{
			"/s/1": "def0", "/s/2": "aaaa", "/s/3": "bbbb", "/s/4": "bbbb", "/s/5": "bbbb", "/s/6": "cccc",
		},
		namespaces: map[string]string{
			"/s/1": "apps",                                 // default identity: its namespaces are not the tenant
			"/s/2": "team-a",                               // one tenant, one namespace
			"/s/3": "team-c", "/s/4": "team-b", "/s/5": "", // one Secret copied into two namespaces
			"/s/6": "", // PV provisioned before the namespace was recorded
		},
	}

	got := map[string]string{}
	for _, target := range p.targets() {
		if target.sup != p.servers[target.identity].sup {
			t.Fatalf("target %s points at another server", target.identity)
		}
		got[target.identity] = target.tenant
	}
	want := map[string]string{"def0": defaultTenant, "aaaa": "team-a", "bbbb": "team-b,team-c", "cccc": ""}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for identity, tenant := range want {
		if got[identity] != tenant {
			t.Fatalf("identity %s: tenant %q, want %q (all: %v)", identity, got[identity], tenant, got)
		}
	}
}

// The merged output must pass client_golang's consistency checks alongside the node plugin's
// own registry, which is how main serves it.
func TestPool_GatherIsConsistentWithTheDriverRegistry(t *testing.T) {
	_, a := metricsServer(t, serving(childMetrics))
	_, b := metricsServer(t, serving(childMetrics))
	p := &Pool{
		servers: map[string]*server{
			"aaaa": {key: "aaaa", sup: a},
			"bbbb": {key: "bbbb", sup: b},
		},
		byPath:     map[string]string{"/s/1": "aaaa", "/s/2": "bbbb"},
		namespaces: map[string]string{"/s/1": "team-a", "/s/2": "team-a"},
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "retyc_csi_build_info", Help: "x"}))

	count, err := testutil.GatherAndCount(prometheus.Gatherers{registry, p})
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	// build_info + 2 counters + 2 histograms + 2 up
	if count != 7 {
		t.Fatalf("got %d series, want 7", count)
	}
}

func TestPool_GatherEmptyPool(t *testing.T) {
	families, err := NewPool(context.Background(), "retyc", nil, "127.0.0.1", 1, t.TempDir()).Gather()
	if err != nil || len(families) != 0 {
		t.Fatalf("empty pool: %d families, %v", len(families), err)
	}
}
