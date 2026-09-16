package webdavsvc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/retyc/retyc-k8s-csi/internal/identity"
)

func TestPool_OneServerPerIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPool(ctx, fakeBinary(t, "exec sleep 30"), []string{"PATH=/bin"}, "127.0.0.1", 40000, t.TempDir())

	a := &identity.Credentials{Token: "ta", Passphrase: "pa"}
	b := &identity.Credentials{Token: "tb", Passphrase: "pb"}

	s1, err := p.Acquire("/stage/vol1", a, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := p.Acquire("/stage/vol2", a, "")
	if err != nil || s2 != s1 {
		t.Fatalf("same identity must share one server: %v %v", s2 == s1, err)
	}
	if again, _ := p.Acquire("/stage/vol1", a, "team-a"); again != s1 || p.servers[a.Key()].refs != 2 {
		t.Fatalf("re-acquiring a staged path must be idempotent, refs=%d", p.servers[a.Key()].refs)
	}

	s3, err := p.Acquire("/stage/vol3", b, "")
	if err != nil || s3 == s1 {
		t.Fatalf("different identity must get its own server: %+v, %v", s3, err)
	}
	ports := map[int]bool{s1.Port: true, s1.MetricsPort: true, s3.Port: true, s3.MetricsPort: true}
	if len(ports) != 4 {
		t.Fatalf("every server needs its own WebDAV and probes ports: %+v, %+v", s1, s3)
	}
	waitFor(t, "servers running", func() bool { return s1.Status().Running && s3.Status().Running })

	if p.namespaces["/stage/vol1"] != "team-a" || p.namespaces["/stage/vol3"] != "" {
		t.Fatalf("each staged path must keep its claim's namespace: %v", p.namespaces)
	}
	if _, err := p.Acquire("/stage/vol3", b, "team-b"); err != nil || p.namespaces["/stage/vol3"] != "team-b" ||
		p.servers[b.Key()].refs != 1 {
		t.Fatalf("a retried stage updates the namespace without another reference: %v, %v", p.namespaces, err)
	}
	// Re-staging a path with other credentials moves it, namespace included, to the other server.
	if _, err := p.Acquire("/stage/vol4", b, "team-b"); err != nil {
		t.Fatal(err)
	}
	if moved, err := p.Acquire("/stage/vol4", a, "team-c"); err != nil || moved != s1 ||
		p.namespaces["/stage/vol4"] != "team-c" || p.servers[b.Key()].refs != 1 || p.servers[a.Key()].refs != 3 {
		t.Fatalf("re-attach: refs a=%d b=%d, namespaces %v, %v",
			p.servers[a.Key()].refs, p.servers[b.Key()].refs, p.namespaces, err)
	}
	p.Release("/stage/vol4")
	p.Release("/stage/vol1")
	if _, ok := p.namespaces["/stage/vol1"]; ok {
		t.Fatal("releasing a path must forget its namespace")
	}
	if _, ok := p.servers[a.Key()]; !ok {
		t.Fatal("server must survive while another volume references it")
	}
	p.Release("/stage/vol2")
	if _, ok := p.servers[a.Key()]; ok {
		t.Fatal("server must stop when its last volume is released")
	}
	waitFor(t, "server a stopped", func() bool { return !s1.Status().Running })
	p.Release("/stage/unknown") // staged before a restart: ignored
}

func TestPool_DefaultIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateDir := t.TempDir()
	// A config directory set on the pod must not be shared by the identities' servers.
	p := NewPool(ctx, fakeBinary(t, "exec sleep 30"), []string{identity.ConfigDirKey + "=/shared"}, "127.0.0.1", 41000,
		stateDir)

	if _, err := p.Acquire("/stage/v", nil, ""); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no default identity: want ErrNoCredentials, got %v", err)
	}
	creds := &identity.Credentials{Token: "t", Passphrase: "p"}
	def, err := p.Pin(creds)
	if err != nil || p.Default() != def {
		t.Fatalf("Pin: %v", err)
	}
	got, err := p.Acquire("/stage/v", nil, "")
	if err != nil || got != def {
		t.Fatalf("nil credentials must resolve to the pinned default: %v", err)
	}
	p.Release("/stage/v")
	if p.Default() == nil {
		t.Fatal("pinned server must not stop on release")
	}
	env := def.Env
	wantConfigDir := identity.ConfigDirKey + "=" + filepath.Join(stateDir, creds.Key(), "config", "retyc")
	found := 0
	for _, kv := range env {
		switch kv {
		case identity.TokenKey + "=t", identity.PassphraseKey + "=p", wantConfigDir:
			found++
		}
	}
	if found != 3 {
		t.Fatalf("server env must carry the identity, got %v", env)
	}
}
