package webdavsvc

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"
)

// scrapeTimeout bounds one server's /metrics round-trip, well inside Prometheus' default 10 s
// scrape timeout since the servers are scraped concurrently.
const scrapeTimeout = 3 * time.Second

// upMetric is set per server by Gather, the way Prometheus sets `up` per target.
const upMetric = "retyc_csi_webdav_server_up"

// Labels added to every merged series. identity is the server's identity key, constant for its
// life and unique in the pool, so series never collide; tenant says who uses it, for humans.
const (
	identityLabel = "identity"
	tenantLabel   = "tenant"
)

// defaultTenant is the tenant of the driver's cluster-wide identity. Namespace names are RFC 1123
// labels, so it never reads as a namespace.
const defaultTenant = "_default"

// Gather implements prometheus.Gatherer: the /metrics of every server in the pool, merged by
// family with identity and tenant labels (see targets), plus retyc_csi_webdav_server_up. A server
// that is down or does not answer only turns its up series to 0: the node plugin's own metrics and
// the other servers' are still served.
func (p *Pool) Gather() ([]*dto.MetricFamily, error) {
	p.mu.Lock()
	targets := p.targets()
	p.mu.Unlock()

	// prometheus.Gatherer carries no context: each scrape is bounded by scrapeTimeout instead, and
	// the handler in main sets no promhttp timeout that could give up before it.
	return gatherServers(context.Background(), targets), nil
}

// scrapeTarget is one server of the pool with the labels of its series.
type scrapeTarget struct {
	identity, tenant string
	sup              *Supervisor
}

// targets lists the pool's servers (locked). The tenant is "_default" for the pinned cluster-wide
// identity, otherwise the namespaces of the volumes staged through the server, sorted and
// comma-separated, or "" when no volume recorded one (PVs provisioned before the driver stored the
// namespace).
func (p *Pool) targets() []scrapeTarget {
	namespaces := map[string]map[string]bool{}
	for path, key := range p.byPath {
		if ns := p.namespaces[path]; ns != "" {
			if namespaces[key] == nil {
				namespaces[key] = map[string]bool{}
			}
			namespaces[key][ns] = true
		}
	}

	out := make([]scrapeTarget, 0, len(p.servers))
	for key, srv := range p.servers {
		tenant := ""
		if srv.pinned {
			tenant = defaultTenant
		} else if len(namespaces[key]) > 0 {
			names := make([]string, 0, len(namespaces[key]))
			for ns := range namespaces[key] {
				names = append(names, ns)
			}
			sort.Strings(names)
			tenant = strings.Join(names, ",")
		}
		out = append(out, scrapeTarget{identity: key, tenant: tenant, sup: srv.sup})
	}

	return out
}

// gatherServers scrapes targets concurrently and merges their families, each series labeled with
// its server's identity and tenant.
func gatherServers(ctx context.Context, targets []scrapeTarget) []*dto.MetricFamily {
	type result struct {
		scrapeTarget
		families map[string]*dto.MetricFamily
	}
	results := make(chan result, len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !target.sup.Status().Running {
				results <- result{scrapeTarget: target}

				return
			}
			families, err := target.sup.scrapeMetrics(ctx)
			if err != nil {
				klog.V(4).Infof("webdavsvc: scraping metrics of identity %s: %v", target.identity, err)
			}
			results <- result{scrapeTarget: target, families: families}
		}()
	}
	wg.Wait()
	close(results)

	merged := map[string]*dto.MetricFamily{}
	up := &dto.MetricFamily{
		Name: proto.String(upMetric),
		Help: proto.String("1 when the retyc webdav serve of this identity answered its /metrics scrape, 0 otherwise."),
		Type: dto.MetricType_GAUGE.Enum(),
	}
	for r := range results {
		value := 0.0
		if r.families != nil {
			value = 1
		}
		up.Metric = append(up.Metric, &dto.Metric{
			Label: r.labels(nil),
			Gauge: &dto.Gauge{Value: proto.Float64(value)},
		})
		for name, mf := range r.families {
			for _, m := range mf.GetMetric() {
				m.Label = r.labels(m.GetLabel())
			}
			existing, ok := merged[name]
			if !ok {
				merged[name] = mf

				continue
			}
			if existing.GetType() != mf.GetType() {
				// Every server runs the same retyc binary, so only a bug gets here. HELP strings are not
				// compared for the same reason.
				klog.Warningf("webdavsvc: metric %s of identity %s is a %s, another server's is a %s: dropped",
					name, r.identity, mf.GetType(), existing.GetType())

				continue
			}
			existing.Metric = append(existing.Metric, mf.Metric...)
		}
	}
	if len(up.Metric) > 0 {
		merged[upMetric] = up
	}

	out := make([]*dto.MetricFamily, 0, len(merged))
	for _, mf := range merged {
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })

	return out
}

// labels returns labels with the target's identity and tenant set, kept sorted by name as
// client_golang expects. Existing labels of those names are overwritten: they are reserved (a
// RETYC_WEBDAV_METRICS_LABELS inherited by the servers does not survive).
func (t scrapeTarget) labels(labels []*dto.LabelPair) []*dto.LabelPair {
	set := map[string]string{identityLabel: t.identity, tenantLabel: t.tenant}
	out := make([]*dto.LabelPair, 0, len(labels)+len(set))
	for _, l := range labels {
		if _, reserved := set[l.GetName()]; !reserved {
			out = append(out, l)
		}
	}
	for name, value := range set {
		out = append(out, &dto.LabelPair{Name: proto.String(name), Value: proto.String(value)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })

	return out
}

// scrapeMetrics GETs the child's /metrics (text exposition format) and parses it.
func (s *Supervisor) scrapeMetrics(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	url := "http://" + s.metricsHostPort() + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d on %s", resp.StatusCode, url)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", url, err)
	}

	return families, nil
}
