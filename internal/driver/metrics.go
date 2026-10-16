package driver

import "github.com/prometheus/client_golang/prometheus"

// NodeMetrics are the node plugin's gauges on its mount pods, updated by every Recover round.
type NodeMetrics struct {
	pods     *prometheus.GaugeVec
	outdated *prometheus.GaugeVec
}

// NewNodeMetrics returns the gauges, to register with Collectors.
func NewNodeMetrics() *NodeMetrics {
	return &NodeMetrics{
		pods: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "retyc_csi_mount_pods",
			Help: "Mount pods of this node by state: pending, running (not ready), ready, terminating, exited.",
		}, []string{"state"}),
		outdated: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "retyc_csi_mount_pod_outdated",
			Help: "Mount pods of this node that run another retyc-cli version than the configured one; " +
				"they take it at the next remount of their volume.",
		}, []string{"reason"}),
	}
}

// Collectors returns the gauges to register.
func (m *NodeMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.pods, m.outdated}
}

func (m *NodeMetrics) update(states []string, outdated int) {
	if m == nil {
		return
	}
	counts := map[string]float64{
		statePending: 0, stateRunning: 0, stateReady: 0, stateTerminating: 0, stateExited: 0,
	}
	for _, s := range states {
		if s != "" {
			counts[s]++
		}
	}
	for state, n := range counts {
		m.pods.WithLabelValues(state).Set(n)
	}
	m.outdated.WithLabelValues("cli_version").Set(float64(outdated))
}
