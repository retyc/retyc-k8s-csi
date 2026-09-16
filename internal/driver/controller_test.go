package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestCreateVolumeResponse_RecordsTheClaimNamespace(t *testing.T) {
	resp := createVolumeResponse("id", "pvc-1", &csi.CreateVolumeRequest{
		Parameters: map[string]string{pvcNamespaceParameter: "team-a"},
	})
	ctx := resp.GetVolume().GetVolumeContext()
	if ctx[volumeContextTitle] != "pvc-1" || ctx[volumeContextNamespace] != "team-a" {
		t.Fatalf("volume context = %v", ctx)
	}

	// Without --extra-create-metadata the provisioner sends no namespace: nothing to record.
	resp = createVolumeResponse("id", "pvc-1", &csi.CreateVolumeRequest{})
	if _, ok := resp.GetVolume().GetVolumeContext()[volumeContextNamespace]; ok {
		t.Fatalf("volume context = %v, want no namespace key", resp.GetVolume().GetVolumeContext())
	}
}

func TestClaimNamespace_OnlyNamespaceNames(t *testing.T) {
	for value, want := range map[string]string{
		"team-a":   "team-a",
		"":         "",
		"_default": "", // would read as the cluster-wide identity's tenant
		"a,b":      "",
		"Team-A":   "",
		"-team":    "",
	} {
		if got := claimNamespace(map[string]string{volumeContextNamespace: value}); got != want {
			t.Errorf("claimNamespace(%q) = %q, want %q", value, got, want)
		}
	}
}
