// Command retycfs-k8s-csi is the entrypoint for both the CSI Controller plugin (--mode=controller)
// and the CSI Node plugin (--mode=node) of the RetycFS CSI driver. See doc/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/driver"
	"github.com/retyc/retycfs-k8s-csi/internal/health"
	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mount"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

func main() {
	var (
		mode     = flag.String("mode", "", "plugin mode: controller | node")
		endpoint = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		retycBin = flag.String("retyc-bin", "retyc", "path to the retyc CLI binary")
		nodeID   = flag.String("node-id", "",
			"Kubernetes name of the node, reported by NodeGetInfo (node mode only, default: $NODE_ID, $NODE_NAME)")
		mountConfig = flag.String("mount-config", "/etc/retycfs-csi/mount.json",
			"JSON configuration of the mount pods: image, resources, drain timeout... (node mode only)")
		namespace = flag.String("namespace", "",
			"namespace of the mount pods, the driver's (node mode only, default: $POD_NAMESPACE)")
		httpEndpoint = flag.String("http-endpoint", ":9808",
			"address for the /healthz (liveness), /readyz (readiness) and /metrics HTTP endpoints; empty disables them")
	)
	klog.InitFlags(nil)
	flag.Parse()

	opts := options{
		mode: *mode, endpoint: *endpoint, retycBin: *retycBin, nodeID: *nodeID,
		mountConfig: *mountConfig, namespace: *namespace, httpEndpoint: *httpEndpoint,
	}
	if err := run(opts); err != nil {
		klog.Fatal(err)
	}
}

type options struct {
	mode, endpoint, retycBin, nodeID, mountConfig, namespace, httpEndpoint string
}

// authStatusCacheTTL bounds how often the controller's readiness re-runs `retyc auth status`
// (one auth-server round-trip) — kubelet probes every few seconds, the token doesn't change.
const authStatusCacheTTL = 2 * time.Minute

func run(o options) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	server := grpc.NewServer()
	var ready health.Checker
	// Node mode only: the reconciliation of the mount pods with the mounts.
	var (
		recoverPods func(context.Context)
		watchPods   func(context.Context, time.Duration)
	)
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "retycfs_csi_build_info",
			Help:        "Constant 1, labeled with the driver version and the plugin mode.",
			ConstLabels: prometheus.Labels{"version": driver.DriverVersion, "mode": o.mode},
		}, func() float64 { return 1 }),
	)
	// The pod's Secret (envFrom) provides the cluster-wide default identity, if any; tenants'
	// identities arrive per request through CSI secrets. The CLI's key cache is off for every
	// child: it keeps an unlocked AGE key in the kernel session keyring under one fixed name,
	// which all the children of this process share whatever identity they run as.
	env := identity.MergeEnv(os.Environ(), map[string]string{identity.KeyringEnabledKey: "false"})
	defaultIdentity := identity.FromEnv(env)

	switch o.mode {
	case "controller":
		retyc := retycclient.New(o.retycBin, env)
		if defaultIdentity != nil {
			ready = health.Cached(retyc.AuthStatusCheck, authStatusCacheTTL)
		} else {
			klog.Info("no default identity in the environment: only StorageClasses with secret parameters will work")
		}
		kube, err := inClusterClient()
		if err != nil {
			return err
		}
		csi.RegisterControllerServer(server, &driver.ControllerServer{Retyc: retyc, Kube: kube})
	case "node":
		node, err := nodeServer(o, defaultIdentity)
		if err != nil {
			return err
		}
		registry.MustRegister(node.Metrics.Collectors()...)
		recoverPods, watchPods = node.Recover, node.Watch
		// /readyz depends on no mount pod: the node plugin serves even when a volume cannot mount.
		csi.RegisterNodeServer(server, node)
	default:
		return fmt.Errorf("--mode must be \"controller\" or \"node\", got %q", o.mode)
	}
	csi.RegisterIdentityServer(server, &driver.IdentityServer{Ready: ready})

	if o.httpEndpoint != "" {
		// The endpoint listens on the pod IP for kubelet's probes: bound concurrent scrapes.
		metrics := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			ErrorLog:            klogPrinter{},
			MaxRequestsInFlight: 2,
		})
		go func() {
			if err := health.ListenAndServe(ctx, o.httpEndpoint, health.Handler(ready, metrics)); err != nil {
				klog.Errorf("HTTP endpoints: %v", err)
			}
		}()
	}

	// Before the CSI socket, so that kubelet's first calls find the mounts repaired; bounded, since
	// it only matters for volumes whose mount pod died while the node plugin was down.
	if recoverPods != nil {
		recoverCtx, cancelRecover := context.WithTimeout(ctx, time.Minute)
		recoverPods(recoverCtx)
		cancelRecover()
		go watchPods(ctx, driver.RecoverInterval)
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

// nodeServer builds the node plugin: its mount pods manager from --mount-config, the node name and
// the driver namespace from the downward API.
func nodeServer(o options, defaultIdentity *identity.Credentials) (*driver.NodeServer, error) {
	nodeID := o.nodeID
	for _, v := range []string{"NODE_ID", "NODE_NAME"} { // the downward API's spec.nodeName
		if nodeID == "" {
			nodeID = os.Getenv(v)
		}
	}
	if nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("determining node ID: %w", err)
		}
		nodeID = hostname
	}
	namespace := o.namespace
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return nil, errors.New("the driver namespace is unknown: set --namespace or $POD_NAMESPACE")
	}
	cfg, cache, err := mountpod.LoadConfig(o.mountConfig)
	if err != nil {
		return nil, err
	}
	cfg.Namespace = namespace
	// From a Secret, since a Redis URL usually holds a password.
	if url := os.Getenv("RETYC_CACHE_REDIS_URL"); url != "" {
		cache.RedisURL = url
	}
	if defaultIdentity == nil {
		klog.Info("no default identity in the environment: only StorageClasses with secret parameters will work")
	}
	kube, err := inClusterClient()
	if err != nil {
		return nil, err
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	pods := mountpod.New(kube, cfg)
	node := &driver.NodeServer{
		NodeID:  nodeID,
		Mounter: mount.New(),
		Pods:    pods,
		Kube:    kube,
		Events: broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
			Component: driver.DriverName + "-node", Host: nodeID,
		}),
		DefaultCredentials: defaultIdentity,
		Cache:              cache,
		Metrics:            driver.NewNodeMetrics(),
	}
	pods.Prepare = node.Prepare
	klog.Infof("node %s: mount pods in namespace %s, image %s", nodeID, namespace, pods.Config().Image)

	return node, nil
}

// inClusterClient builds a Kubernetes client from the pod's service account. The controller reads
// the claims it provisions volumes for (their annotations); the node plugin reads the claims it
// stages and manages the mount pods.
func inClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the in-cluster Kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating the Kubernetes client: %w", err)
	}

	return client, nil
}

// klogPrinter logs the errors promhttp continues past (a family dropped as inconsistent) as warnings.
type klogPrinter struct{}

func (klogPrinter) Println(v ...any) { klog.Warning(v...) }

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
