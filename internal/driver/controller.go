package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/retyc/retyc-k8s-csi/internal/retycclient"
)

// ControllerServer implements csi.ControllerServer by execing the retyc CLI. 1 CSI volume = 1
// Retyc dataroom, titled after the CSI-generated volume name (req.GetName(), always unique —
// see plan: WebDAV exposes datarooms by title, so this also fixes the mount path deterministically
// without a title→ID lookup at NodeStageVolume time).
type ControllerServer struct {
	csi.UnimplementedControllerServer
	Retyc *retycclient.Client
}

func (s *ControllerServer) ControllerGetCapabilities(
	context.Context, *csi.ControllerGetCapabilitiesRequest,
) (*csi.ControllerGetCapabilitiesResponse, error) {
	capability := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		}
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			capability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		},
	}, nil
}

// isSupportedCapability accepts any filesystem-mount access mode (single- or multi-node); block
// volumes are rejected — a davfs2 mount is a filesystem, not a raw device.
func isSupportedCapability(cap *csi.VolumeCapability) bool {
	if cap.GetBlock() != nil {
		return false
	}
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return true
	default:
		return false
	}
}

func (s *ControllerServer) CreateVolume(
	ctx context.Context, req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedCapability(cap) {
			return nil, status.Error(codes.InvalidArgument, "only filesystem-mount volume capabilities are supported")
		}
	}

	// Idempotency (CSI requires CreateVolume to be safely retriable): if a dataroom titled `name`
	// already exists, reuse it instead of creating a duplicate. Best-effort on two counts: there's
	// no unique constraint on the backend (a racing double-create is still possible), and `retyc
	// dataroom ls` only returns page 1 (see retycclient.DataroomList.Complete) — past that, a
	// retried CreateVolume for a dataroom on a later page would create a duplicate. Acceptable for
	// accepted for now; logged loudly so it's visible when it starts to matter.
	existing, err := s.Retyc.ListDatarooms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing datarooms: %v", err)
	}
	for _, dr := range existing.Items {
		if dr.Title == name {
			klog.Infof("CreateVolume: reusing existing dataroom %q for volume %q", dr.ID, name)

			return createVolumeResponse(dr.ID, dr.Title, req), nil
		}
	}
	if !existing.Complete {
		klog.Warningf("CreateVolume: dataroom listing is paginated and only page 1 was checked; "+
			"a retried create for %q may produce a duplicate dataroom", name)
	}

	quota, err := s.Retyc.Quota(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking quota: %v", err)
	}
	if quota.IsUploadReadOnly {
		return nil, status.Error(codes.ResourceExhausted, "retyc account storage quota exceeded (read-only)")
	}
	if !quota.HasDataroomQuota() {
		return nil, status.Errorf(codes.ResourceExhausted, "retyc account dataroom limit reached (%d/%d)",
			quota.CountDataroom, *quota.MaxCountDataroom)
	}

	result, err := s.Retyc.CreateDataroom(ctx, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "creating dataroom: %v", err)
	}
	klog.Infof("CreateVolume: created dataroom %q for volume %q", result.ID, name)

	return createVolumeResponse(result.ID, result.Title, req), nil
}

// createVolumeResponse builds the CSI response for a dataroom. Requested capacity is echoed back
// unmodified (best-effort: Retyc has no per-dataroom size cap to enforce it
// against). VolumeContext carries the dataroom title so NodeStageVolume can build the WebDAV
// mount path without a separate ID→title lookup.
func createVolumeResponse(id, title string, req *csi.CreateVolumeRequest) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      id,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: map[string]string{"title": title},
		},
	}
}

func (s *ControllerServer) DeleteVolume(
	ctx context.Context, req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	_, err := s.Retyc.DeleteDataroom(ctx, id)
	if err != nil {
		// DeleteVolume must be idempotent: a dataroom already gone (e.g. a retried call after a
		// prior successful delete) is success, not an error.
		if retycclient.IsNotFound(err) {
			klog.Infof("DeleteVolume: dataroom %q already gone, treating as success", id)

			return &csi.DeleteVolumeResponse{}, nil
		}

		return nil, status.Errorf(codes.Internal, "deleting dataroom %q: %v", id, err)
	}
	klog.Infof("DeleteVolume: deleted dataroom %q", id)

	return &csi.DeleteVolumeResponse{}, nil
}

func (s *ControllerServer) ValidateVolumeCapabilities(
	_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedCapability(cap) {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: "only filesystem-mount volume capabilities are supported",
			}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}
