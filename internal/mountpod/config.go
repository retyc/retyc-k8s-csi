package mountpod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// FileConfig is the node-wide mount pod configuration as the chart writes it (a JSON file mounted
// from a ConfigMap, `--mount-config`). Zero fields keep the defaults of New.
type FileConfig struct {
	Image            string                        `json:"image"`
	CLIVersion       string                        `json:"cliVersion,omitempty"`
	ImagePullPolicy  corev1.PullPolicy             `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	ExtraEnv         []corev1.EnvVar               `json:"extraEnv,omitempty"`
	HostAliases      []corev1.HostAlias            `json:"hostAliases,omitempty"`
	HostPID          bool                          `json:"hostPID,omitempty"`
	Resources        corev1.ResourceRequirements   `json:"resources,omitzero"`
	// PriorityClassName "" keeps system-node-critical.
	PriorityClassName string `json:"priorityClassName,omitempty"`
	KubeletDir        string `json:"kubeletDir,omitempty"`
	SpoolDir          string `json:"spoolDir,omitempty"`
	// DrainTimeout is a Go duration ("5m").
	DrainTimeout string `json:"drainTimeout,omitempty"`
	MetricsPort  int    `json:"metricsPort,omitempty"`
	// Redis selects the shared listing cache; an empty URL keeps the CLI's memory cache. The URL
	// may carry a password: the node plugin's RETYC_CACHE_REDIS_URL, from a Secret, overrides it.
	Redis struct {
		URL                 string `json:"url,omitempty"`
		HighConfidentiality bool   `json:"highConfidentiality,omitempty"`
	} `json:"redis,omitzero"`
}

// LoadConfig reads a FileConfig. Unknown keys are refused, so that a misspelled setting fails the
// start instead of being ignored.
func LoadConfig(path string) (Config, Cache, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is a command-line flag
	if err != nil {
		return Config{}, Cache{}, fmt.Errorf("reading the mount pod config: %w", err)
	}

	return ParseConfig(data)
}

// ParseConfig decodes a FileConfig and converts it to the Manager's Config and the cache settings
// of the specs.
func ParseConfig(data []byte) (Config, Cache, error) {
	var fc FileConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return Config{}, Cache{}, fmt.Errorf("parsing the mount pod config: %w", err)
	}
	if fc.Image == "" {
		return Config{}, Cache{}, fmt.Errorf("mount pod config: image is required")
	}
	cfg := Config{
		Image: fc.Image, CLIVersion: fc.CLIVersion, ImagePullPolicy: fc.ImagePullPolicy,
		ImagePullSecrets: fc.ImagePullSecrets, ExtraEnv: fc.ExtraEnv, HostAliases: fc.HostAliases,
		HostPID: fc.HostPID, Resources: fc.Resources, PriorityClassName: fc.PriorityClassName,
		KubeletDir: fc.KubeletDir, SpoolRoot: fc.SpoolDir, MetricsPort: fc.MetricsPort,
	}
	if fc.DrainTimeout != "" {
		d, err := time.ParseDuration(fc.DrainTimeout)
		if err != nil || d <= 0 {
			return Config{}, Cache{}, fmt.Errorf("mount pod config: drainTimeout %q: want a positive duration", fc.DrainTimeout)
		}
		cfg.DrainTimeout = d
	}

	return cfg, Cache{RedisURL: fc.Redis.URL, HighConfidentiality: fc.Redis.HighConfidentiality}, nil
}
