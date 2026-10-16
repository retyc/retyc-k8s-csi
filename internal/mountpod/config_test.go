package mountpod

import (
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	cfg, cache, err := ParseConfig([]byte(`{
		"image": "retyc-cli:retycfs-v1.4.0", "imagePullPolicy": "IfNotPresent",
		"imagePullSecrets": [{"name": "regcred"}],
		"extraEnv": [{"name": "RETYC_INSECURE", "value": "true"}],
		"hostAliases": [{"ip": "192.168.121.1", "hostnames": ["api.example"]}],
		"resources": {"requests": {"memory": "256Mi"}, "limits": {"memory": "512Mi"}},
		"drainTimeout": "2m", "kubeletDir": "/k", "spoolDir": "/s",
		"redis": {"url": "redis://r:6379/0", "highConfidentiality": true}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "retyc-cli:retycfs-v1.4.0" || cfg.ImagePullPolicy != "IfNotPresent" ||
		len(cfg.ImagePullSecrets) != 1 || len(cfg.ExtraEnv) != 1 || len(cfg.HostAliases) != 1 ||
		cfg.Resources.Limits.Memory().String() != "512Mi" || cfg.DrainTimeout != 2*time.Minute ||
		cfg.KubeletDir != "/k" || cfg.SpoolRoot != "/s" {
		t.Errorf("config = %+v", cfg)
	}
	if cache.RedisURL != "redis://r:6379/0" || !cache.HighConfidentiality {
		t.Errorf("cache = %+v", cache)
	}
	m := New(nil, cfg)
	if m.Config().CLIVersion != "retycfs-v1.4.0" {
		t.Errorf("CLI version = %q", m.Config().CLIVersion)
	}

	for _, bad := range []string{`{}`, `{"image": "x", "imag": "y"}`, `{"image": "x", "drainTimeout": "-1s"}`} {
		if _, _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("ParseConfig(%s) accepted", bad)
		}
	}
}
