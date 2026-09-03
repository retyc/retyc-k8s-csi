package driver

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/retyc/retyc-k8s-csi/internal/mount"
	"github.com/retyc/retyc-k8s-csi/internal/webdavsvc"
)

// NodeServer implements csi.NodeServer. NodeStageVolume mounts a dataroom via davfs2 against the
// node-local `retyc webdav serve` process (supervised by Webdav); NodePublishVolume bind-mounts
// that staging path into each pod's target path — the standard CSI pattern that lets several
// pods on the same node share one davfs2 mount.
type NodeServer struct {
	csi.UnimplementedNodeServer
	NodeID  string
	Mounter *mount.Mounter
	Webdav  *webdavsvc.Supervisor
}

func (s *NodeServer) NodeGetCapabilities(
	context.Context, *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	capability := func(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
		return &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: t},
			},
		}
	}

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			capability(csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME),
		},
	}, nil
}

func (s *NodeServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.NodeID}, nil
}

// webdavReadyTimeout caps how long NodeStageVolume waits for the local webdav server.
const webdavReadyTimeout = 30 * time.Second

// dataroomURL builds the davfs2 mount source for a dataroom, keyed by title (see plan: WebDAV
// exposes datarooms under /dataroom/<title>, not by ID).
func (s *NodeServer) dataroomURL(title string) string {
	return fmt.Sprintf("%s/dataroom/%s", s.Webdav.BaseURL(), url.PathEscape(title))
}

func (s *NodeServer) NodeStageVolume(
	ctx context.Context, req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	title := req.GetVolumeContext()["title"]
	if title == "" {
		return nil, status.Error(codes.InvalidArgument,
			"volume_context[\"title\"] is required (set by ControllerServer.CreateVolume)")
	}

	// Bound the wait ourselves: kubelet's CSI calls carry a deadline, but csi-sanity and manual
	// grpc clients may not, and a webdav server that never comes up (bad RETYC_TOKEN) must
	// surface as Unavailable, not hang the RPC forever.
	readyCtx, cancel := context.WithTimeout(ctx, webdavReadyTimeout)
	defer cancel()
	if err := s.Webdav.WaitReady(readyCtx); err != nil {
		return nil, status.Errorf(codes.Unavailable, "local webdav server not ready: %v", err)
	}

	dataroomURL := s.dataroomURL(title)
	klog.Infof("NodeStageVolume: mounting %s at %s", dataroomURL, stagingPath)
	if err := s.Mounter.MountDavfs(dataroomURL, stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "mounting dataroom: %v", err)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnstageVolume(
	_ context.Context, req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	if err := s.Mounter.UnmountDavfs(stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmounting %s: %v", stagingPath, err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *NodeServer) NodePublishVolume(
	_ context.Context, req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	source := req.GetStagingTargetPath()
	target := req.GetTargetPath()
	if source == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "staging and target paths are required")
	}

	klog.Infof("NodePublishVolume: bind-mounting %s at %s (readonly=%t)", source, target, req.GetReadonly())
	if err := s.Mounter.BindMount(source, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing volume: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnpublishVolume(
	_ context.Context, req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if err := s.Mounter.Unpublish(target); err != nil {
		return nil, status.Errorf(codes.Internal, "unpublishing %s: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
