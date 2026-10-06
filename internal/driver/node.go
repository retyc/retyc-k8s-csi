package driver

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"

	"github.com/retyc/retyc-k8s-csi/internal/identity"
	"github.com/retyc/retyc-k8s-csi/internal/mount"
	"github.com/retyc/retyc-k8s-csi/internal/webdavsvc"
)

// NodeServer implements csi.NodeServer. NodeStageVolume mounts a dataroom via davfs2 against the
// node-local `retyc webdav serve` of the volume's identity (one supervised server per identity,
// see webdavsvc.Pool); NodePublishVolume bind-mounts that staging path into each pod's target
// path — the standard CSI pattern that lets several pods on the same node share one davfs2 mount.
type NodeServer struct {
	csi.UnimplementedNodeServer
	NodeID  string
	Mounter *mount.Mounter
	Webdav  *webdavsvc.Pool
	// Stages records the staged volumes for Recover; nil disables the recovery.
	Stages *StageStore

	locks keyLock // by staging path
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

// dataroomURL builds the davfs2 mount source for a dataroom on a given server, keyed by title
// (the WebDAV server exposes datarooms under /dataroom/<title>, not by ID).
func dataroomURL(server *webdavsvc.Supervisor, title string) string {
	return fmt.Sprintf("%s/dataroom/%s", server.BaseURL(), url.PathEscape(title))
}

// namespaceName is a Kubernetes namespace name (RFC 1123 label).
var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// claimNamespace returns the claim namespace recorded in a volume context, or "" when it is absent
// or not a namespace name. A static PV's attributes are free text: an invalid value must not end up
// in the tenant label, where "_default" or a comma already mean something.
func claimNamespace(volumeContext map[string]string) string {
	ns := volumeContext[volumeContextNamespace]
	if ns != "" && !namespaceName.MatchString(ns) {
		klog.Warningf("ignoring volume_context[%q] = %q: not a namespace name", volumeContextNamespace, ns)

		return ""
	}

	return ns
}

func (s *NodeServer) NodeStageVolume(
	ctx context.Context, req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	title := req.GetVolumeContext()[volumeContextTitle]
	if title == "" {
		return nil, status.Error(codes.InvalidArgument,
			"volume_context[\"title\"] is required (set by ControllerServer.CreateVolume)")
	}

	creds, err := identity.FromSecrets(req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer s.locks.Lock(stagingPath)()
	server, err := s.Webdav.Acquire(stagingPath, creds, claimNamespace(req.GetVolumeContext()))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// Bound the wait ourselves: kubelet's CSI calls carry a deadline, but csi-sanity and manual
	// grpc clients may not, and a webdav server that never comes up (bad RETYC_TOKEN) must
	// surface as Unavailable, not hang the RPC forever. On failure the server reference is
	// dropped again so a crash-looping tenant server does not outlive kubelet's retries.
	readyCtx, cancel := context.WithTimeout(ctx, webdavReadyTimeout)
	defer cancel()
	if err := server.WaitReady(readyCtx); err != nil {
		s.Webdav.Release(stagingPath)

		return nil, status.Errorf(codes.Unavailable, "local webdav server not ready: %v", err)
	}

	source := dataroomURL(server, title)
	klog.Infof("NodeStageVolume: mounting %s at %s", source, stagingPath)
	if err := s.Mounter.MountDavfs(source, stagingPath); err != nil {
		s.Webdav.Release(stagingPath)

		return nil, status.Errorf(codes.Internal, "mounting dataroom: %v", err)
	}
	s.recordStage(stagingPath, title, creds, claimNamespace(req.GetVolumeContext()))

	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnstageVolume(
	_ context.Context, req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	defer s.locks.Lock(stagingPath)()

	if err := s.Mounter.UnmountDavfs(stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmounting %s: %v", stagingPath, err)
	}
	if s.Stages != nil {
		if err := s.Stages.Remove(stagingPath); err != nil {
			klog.Warningf("NodeUnstageVolume: removing the stage record of %s: %v", stagingPath, err)
		}
	}
	s.Webdav.Release(stagingPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *NodeServer) NodePublishVolume(
	ctx context.Context, req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	source := req.GetStagingTargetPath()
	target := req.GetTargetPath()
	if source == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "staging and target paths are required")
	}
	// kubelet does not stage a volume again for a new pod while another pod on the node still
	// uses it: a dead staging mount has to be repaired here, or the pod never starts.
	if err := s.repairStaging(ctx, source); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
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

// recordStage saves what Recover needs to remount stagingPath. A failure only costs the
// recovery of this volume after a plugin restart, not the mount: it is logged, not returned.
func (s *NodeServer) recordStage(stagingPath, title string, creds *identity.Credentials, namespace string) {
	if s.Stages == nil {
		return
	}
	rec := &stageRecord{StagingPath: stagingPath, Title: title, Namespace: namespace}
	if infos, err := mountutils.ParseMountInfo(procMountInfo); err == nil {
		if top, ok := topMount(infos, stagingPath); ok {
			rec.Device = device(top)
		}
	}
	if creds != nil {
		rec.Token, rec.Passphrase = creds.Token, creds.Passphrase
	}
	if err := s.Stages.Save(rec); err != nil {
		klog.Warningf("NodeStageVolume: recording %s (it will not be remounted after a plugin restart): %v",
			stagingPath, err)
	}
}
