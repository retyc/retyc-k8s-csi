// Package driver implements the CSI Identity, Controller and Node gRPC services for the Retyc
// CSI POC. Retyc datarooms, mounted via retyc-cli's WebDAV server + davfs2, back an RWX
// filesystem volume — see /home/triplestack/.claude/plans/buzzing-juggling-fern.md for the full
// design and its accepted POC trade-offs (no resize/snapshot, no per-dataroom capacity
// enforcement, eventually-consistent shared filesystem).
package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// DriverName is advertised to kubelet/external-provisioner and referenced by the StorageClass's
// `provisioner:` field.
const DriverName = "csi.retyc.com"

// DriverVersion is bumped on every behavior-affecting release; kubelet logs it on registration.
const DriverVersion = "0.1.0"

// IdentityServer implements csi.IdentityServer. Shared by both the controller and node plugin
// processes (each runs its own gRPC server on its own unix socket).
type IdentityServer struct {
	csi.UnimplementedIdentityServer
}

func (s *IdentityServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          DriverName,
		VendorVersion: DriverVersion,
	}, nil
}

func (s *IdentityServer) GetPluginCapabilities(
	context.Context, *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

// Probe reports readiness. It's intentionally a static OK: real health (retyc auth validity,
// webdav server liveness) surfaces through CreateVolume/NodeStageVolume errors instead, since
// there's no cheap, side-effect-free liveness check to run here.
func (s *IdentityServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
