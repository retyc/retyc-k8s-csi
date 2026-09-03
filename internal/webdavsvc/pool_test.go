package webdavsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/retyc/retyc-k8s-csi/internal/identity"
)

func TestPool_OneServerPerIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPool(ctx, fakeBinary(t, "exec sleep 30"), []string{"PATH=/bin"}, "127.0.0.1", 40000, t.TempDir())

	a := &identity.Credentials{Token: "ta", Passphrase: "pa"}
	b := &identity.Credentials{Token: "tb", Passphrase: "pb"}

	s1, err := p.Acquire("/stage/vol1", a)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := p.Acquire("/stage/vol2", a)
	if err != nil || s2 != s1 {
		t.Fatalf("same identity must share one server: %v %v", s2 == s1, err)
	}
	if again, _ := p.Acquire("/stage/vol1", a); again != s1 || p.servers[a.Key()].refs != 2 {
		t.Fatalf("re-acquiring a staged path must be idempotent, refs=%d", p.servers[a.Key()].refs)
	}

	s3, err := p.Acquire("/stage/vol3", b)
	if err != nil || s3 == s1 || s3.Port == s1.Port {
		t.Fatalf("different identity must get its own server/port: %+v, %v", s3, err)
	}
	waitFor(t, "servers running", func() bool { return s1.Status().Running && s3.Status().Running })

	p.Release("/stage/vol1")
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
	p := NewPool(ctx, fakeBinary(t, "exec sleep 30"), nil, "127.0.0.1", 41000, t.TempDir())

	if _, err := p.Acquire("/stage/v", nil); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no default identity: want ErrNoCredentials, got %v", err)
	}
	def, err := p.Pin(&identity.Credentials{Token: "t", Passphrase: "p"})
	if err != nil || p.Default() != def {
		t.Fatalf("Pin: %v", err)
	}
	got, err := p.Acquire("/stage/v", nil)
	if err != nil || got != def {
		t.Fatalf("nil credentials must resolve to the pinned default: %v", err)
	}
	p.Release("/stage/v")
	if p.Default() == nil {
		t.Fatal("pinned server must not stop on release")
	}
	env := def.Env
	found := 0
	for _, kv := range env {
		switch kv {
		case identity.TokenKey + "=t", identity.PassphraseKey + "=p":
			found++
		}
	}
	if found != 2 {
		t.Fatalf("server env must carry the identity, got %v", env)
	}
}
