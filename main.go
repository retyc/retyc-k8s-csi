// Command retyc-k8s-csi is the entrypoint for both the CSI Controller plugin (--mode=controller)
// and the CSI Node plugin (--mode=node) of the Retyc RWX CSI POC. See
// /home/triplestack/.claude/plans/buzzing-juggling-fern.md for the architecture.
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

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/retyc/retyc-k8s-csi/internal/driver"
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
		webdavAddr = flag.String("webdav-addr", "127.0.0.1", "bind address for the supervised 'retyc webdav serve' (node mode only)")
		webdavPort = flag.Int("webdav-port", 8888, "port for the supervised 'retyc webdav serve' (node mode only)")
	)
	klog.InitFlags(nil)
	flag.Parse()

	if err := run(*mode, *endpoint, *retycBin, *nodeID, *webdavAddr, *webdavPort); err != nil {
		klog.Fatal(err)
	}
}

func run(mode, endpoint, retycBin, nodeID, webdavAddr string, webdavPort int) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, &driver.IdentityServer{})

	switch mode {
	case "controller":
		env := os.Environ() // carries RETYC_TOKEN / RETYC_KEY_PASSPHRASE from the pod's Secret
		csi.RegisterControllerServer(server, &driver.ControllerServer{
			Retyc: retycclient.New(retycBin, env),
		})
	case "node":
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

		webdav := &webdavsvc.Supervisor{
			BinPath: retycBin,
			Env:     os.Environ(),
			Addr:    webdavAddr,
			Port:    webdavPort,
		}
		go webdav.Run(ctx)

		csi.RegisterNodeServer(server, &driver.NodeServer{
			NodeID:  nodeID,
			Mounter: mount.New(mount.DefaultDavfsOptions),
			Webdav:  webdav,
		})
	default:
		return fmt.Errorf("--mode must be \"controller\" or \"node\", got %q", mode)
	}

	listener, err := listen(endpoint)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		server.GracefulStop()
	}()

	klog.Infof("%s plugin listening on %s", mode, endpoint)

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
