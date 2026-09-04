// Command retyc-k8s-csi is the entrypoint for both the CSI Controller plugin (--mode=controller)
// and the CSI Node plugin (--mode=node) of the Retyc CSI driver. See doc/architecture.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/retyc/retyc-k8s-csi/internal/driver"
	"github.com/retyc/retyc-k8s-csi/internal/health"
	"github.com/retyc/retyc-k8s-csi/internal/identity"
	"github.com/retyc/retyc-k8s-csi/internal/mount"
	"github.com/retyc/retyc-k8s-csi/internal/retycclient"
	"github.com/retyc/retyc-k8s-csi/internal/webdavsvc"
)

func main() {
	var (
		mode       = flag.String("mode", "", "plugin mode: controller | node")
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		retycBin   = flag.String("retyc-bin", "retyc", "path to the retyc CLI binary")
		nodeID     = flag.String("node-id", "", "node ID reported by NodeGetInfo (node mode only, default: hostname)")
		webdavAddr = flag.String("webdav-addr", "127.0.0.1",
			"bind address for the supervised 'retyc webdav serve' (node mode only)")
		webdavPort = flag.Int("webdav-port", 8888,
			"first port for the supervised 'retyc webdav serve' servers, one per identity (node mode only)")
		stateDir = flag.String("state-dir", "/var/lib/retyc-csi",
			"directory for per-identity retyc state (node mode only)")
		httpEndpoint = flag.String("http-endpoint", ":9808",
			"address for the /healthz (liveness) and /readyz (readiness) HTTP endpoints; empty disables them")
	)
	klog.InitFlags(nil)
	flag.Parse()

	opts := options{
		mode: *mode, endpoint: *endpoint, retycBin: *retycBin, nodeID: *nodeID,
		webdavAddr: *webdavAddr, webdavPort: *webdavPort, stateDir: *stateDir, httpEndpoint: *httpEndpoint,
	}
	if err := run(opts); err != nil {
		klog.Fatal(err)
	}
}

type options struct {
	mode, endpoint, retycBin, nodeID, webdavAddr, stateDir, httpEndpoint string
	webdavPort                                                           int
}

// authStatusCacheTTL bounds how often the controller's readiness re-runs `retyc auth status`
// (one auth-server round-trip) — kubelet probes every few seconds, the token doesn't change.
const authStatusCacheTTL = 2 * time.Minute

func run(o options) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	server := grpc.NewServer()
	var ready health.Checker
	// The pod's Secret (envFrom) provides the cluster-wide default identity, if any; tenants'
	// identities arrive per request through CSI secrets.
	env := os.Environ()
	defaultIdentity := identity.FromEnv(env)

	switch o.mode {
	case "controller":
		retyc := retycclient.New(o.retycBin, env)
		if defaultIdentity != nil {
			ready = health.Cached(retyc.AuthStatusCheck, authStatusCacheTTL)
		} else {
			klog.Info("no default identity in the environment: only StorageClasses with secret parameters will work")
		}
		csi.RegisterControllerServer(server, &driver.ControllerServer{Retyc: retyc})
	case "node":
		nodeID := o.nodeID
		if nodeID == "" {
			nodeID = os.Getenv("NODE_ID") // set via the downward API (spec.nodeName) in the DaemonSet
		}
		if nodeID == "" {
			hostname, err := os.Hostname()
			if err != nil {
				return fmt.Errorf("determining node ID: %w", err)
			}
			nodeID = hostname
		}

		pool := webdavsvc.NewPool(ctx, o.retycBin, env, o.webdavAddr, o.webdavPort, o.stateDir)
		if defaultIdentity != nil {
			if _, err := pool.Pin(defaultIdentity); err != nil {
				return fmt.Errorf("starting the default webdav server: %w", err)
			}
		} else {
			klog.Info("no default identity in the environment: only StorageClasses with secret parameters will work")
		}
		ready = pool.Healthy

		csi.RegisterNodeServer(server, &driver.NodeServer{
			NodeID:  nodeID,
			Mounter: mount.New(mount.DefaultDavfsOptions),
			Webdav:  pool,
		})
	default:
		return fmt.Errorf("--mode must be \"controller\" or \"node\", got %q", o.mode)
	}
	csi.RegisterIdentityServer(server, &driver.IdentityServer{Ready: ready})

	if o.httpEndpoint != "" {
		go func() {
			if err := health.ListenAndServe(ctx, o.httpEndpoint, health.Handler(ready)); err != nil {
				klog.Errorf("health endpoints: %v", err)
			}
		}()
	}

	listener, err := listen(o.endpoint)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		server.GracefulStop()
	}()

	klog.Infof("%s plugin listening on %s", o.mode, o.endpoint)

	return server.Serve(listener)
}

// listen parses a unix:// CSI endpoint and removes any stale socket file before binding.
func listen(endpoint string) (net.Listener, error) {
	addr, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return nil, fmt.Errorf("unsupported endpoint scheme %q: only unix:// is supported", endpoint)
	}

	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %s: %w", addr, err)
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	return listener, nil
}
