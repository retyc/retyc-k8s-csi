package webdavsvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"k8s.io/klog/v2"

	"github.com/retyc/retyc-k8s-csi/internal/identity"
)

// Pool runs one supervised `retyc webdav serve` per Retyc identity in use on the node. The
// cluster-wide default identity (from the driver's environment) is pinned for the life of the
// process; per-tenant identities arriving through NodeStageVolume secrets are started on first
// use and stopped when their last staged volume is unstaged. Every server gets its own loopback
// port and its own XDG state directory, so the CLI's caches and tokens never mix.
type Pool struct {
	BinPath string
	// BaseEnv is the environment every server inherits, before identity-specific overrides.
	BaseEnv []string
	Addr    string
	// BasePort is the first loopback port tried; each server takes the next free one.
	BasePort int
	// StateDir holds one sub-directory per identity (HOME + XDG_* of its server).
	StateDir string

	ctx     context.Context //nolint:containedctx // parent of every server's lifetime, set by NewPool
	mu      sync.Mutex
	servers map[string]*server // by identity key
	byPath  map[string]string  // staging path -> identity key
}

type server struct {
	key    string
	sup    *Supervisor
	cancel context.CancelFunc
	refs   int
	pinned bool
}

// NewPool creates an empty pool whose servers live no longer than ctx.
func NewPool(ctx context.Context, binPath string, baseEnv []string, addr string, basePort int, stateDir string) *Pool {
	return &Pool{
		BinPath: binPath, BaseEnv: baseEnv, Addr: addr, BasePort: basePort, StateDir: stateDir,
		ctx:     ctx,
		servers: map[string]*server{},
		byPath:  map[string]string{},
	}
}

// Pin starts the server for creds (if needed) and keeps it running regardless of volume
// references. Used for the cluster-wide default identity.
func (p *Pool) Pin(creds *identity.Credentials) (*Supervisor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	srv, err := p.ensure(creds)
	if err != nil {
		return nil, err
	}
	srv.pinned = true

	return srv.sup, nil
}

// Default returns the pinned default server, or nil when the driver has no default identity.
func (p *Pool) Default() *Supervisor {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, srv := range p.servers {
		if srv.pinned {
			return srv.sup
		}
	}

	return nil
}

// ErrNoCredentials is returned by Acquire when a request carries no secrets and the driver has
// no default identity either.
var ErrNoCredentials = errors.New("no credentials: the request carried no secrets and the driver " +
	"has no default RETYC_TOKEN / RETYC_KEY_PASSPHRASE")

// Acquire returns the server to mount stagingPath against: the one for creds, started if
// needed, or the pinned default when creds is nil. It is idempotent per staging path, so a
// retried NodeStageVolume does not double-count.
func (p *Pool) Acquire(stagingPath string, creds *identity.Credentials) (*Supervisor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if creds == nil {
		for _, srv := range p.servers {
			if srv.pinned {
				return p.attach(stagingPath, srv), nil
			}
		}

		return nil, ErrNoCredentials
	}
	srv, err := p.ensure(creds)
	if err != nil {
		return nil, err
	}

	return p.attach(stagingPath, srv), nil
}

// attach records stagingPath -> srv (locked). Re-attaching the same path to the same server is a
// no-op; to a different server it releases the previous one first.
func (p *Pool) attach(stagingPath string, srv *server) *Supervisor {
	if prev, ok := p.byPath[stagingPath]; ok {
		if prev == srv.key {
			return srv.sup
		}
		p.releaseLocked(stagingPath)
	}
	p.byPath[stagingPath] = srv.key
	srv.refs++

	return srv.sup
}

// Release drops stagingPath's reference; the server stops when nothing references it any more
// (unless pinned). Unknown paths — e.g. staged before a plugin restart — are ignored.
func (p *Pool) Release(stagingPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseLocked(stagingPath)
}

func (p *Pool) releaseLocked(stagingPath string) {
	key, ok := p.byPath[stagingPath]
	if !ok {
		return
	}
	delete(p.byPath, stagingPath)
	srv := p.servers[key]
	srv.refs--
	if srv.refs > 0 || srv.pinned {
		return
	}
	klog.Infof("webdavsvc: stopping server for identity %s (no volumes left)", key)
	srv.cancel()
	delete(p.servers, key)
	if err := os.RemoveAll(p.stateDir(key)); err != nil {
		klog.Warningf("webdavsvc: removing state dir of identity %s: %v", key, err)
	}
}

// ensure returns the running server for creds, starting one if needed (locked).
func (p *Pool) ensure(creds *identity.Credentials) (*server, error) {
	key := creds.Key()
	if srv, ok := p.servers[key]; ok {
		return srv, nil
	}

	port, err := p.freePort()
	if err != nil {
		return nil, err
	}
	dir := p.stateDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir for identity %s: %w", key, err)
	}
	env := creds.EnvOverrides()
	env["HOME"] = dir
	env["XDG_CONFIG_HOME"] = filepath.Join(dir, "config")
	env["XDG_CACHE_HOME"] = filepath.Join(dir, "cache")
	env["XDG_DATA_HOME"] = filepath.Join(dir, "data")

	ctx, cancel := context.WithCancel(p.ctx) //nolint:gosec // G118: stored in server, called by releaseLocked
	srv := &server{
		key:    key,
		cancel: cancel,
		sup: &Supervisor{
			BinPath: p.BinPath,
			Env:     identity.MergeEnv(p.BaseEnv, env),
			Addr:    p.Addr,
			Port:    port,
		},
	}
	klog.Infof("webdavsvc: starting server for identity %s on %s", key, srv.sup.BaseURL())
	go srv.sup.Run(ctx)
	p.servers[key] = srv

	return srv, nil
}

func (p *Pool) stateDir(key string) string {
	return filepath.Join(p.StateDir, key)
}

// freePort returns the first port >= BasePort that no pool server uses and that binds (locked).
func (p *Pool) freePort() (int, error) {
	used := map[int]bool{}
	for _, srv := range p.servers {
		used[srv.sup.Port] = true
	}
	for port := p.BasePort; port < p.BasePort+1000; port++ {
		if used[port] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(p.Addr, fmt.Sprint(port)))
		if err != nil {
			continue
		}
		_ = l.Close()

		return port, nil
	}

	return 0, fmt.Errorf("no free port in [%d, %d) on %s", p.BasePort, p.BasePort+1000, p.Addr)
}

// Healthy reports nil when every server in the pool answers, or one error listing those that
// don't. An empty pool (no default identity, no tenant volumes yet) is healthy.
func (p *Pool) Healthy(ctx context.Context) error {
	p.mu.Lock()
	sups := make(map[string]*Supervisor, len(p.servers))
	for key, srv := range p.servers {
		sups[key] = srv.sup
	}
	p.mu.Unlock()

	var failures []string
	for key, sup := range sups {
		if err := sup.Healthy(ctx); err != nil {
			failures = append(failures, fmt.Sprintf("identity %s: %v", key, err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)

	return errors.New(fmt.Sprint(len(failures), " of ", len(sups), " webdav servers unhealthy: ",
		joinLines(failures)))
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "; "
		}
		out += l
	}

	return out
}
