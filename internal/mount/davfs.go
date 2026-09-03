// Package mount wraps the two mount operations the node plugin needs: mounting a dataroom via
// davfs2 at staging time, and bind-mounting the staging path into a pod's target path at publish
// time. Built on k8s.io/mount-utils, the same library every CSI node plugin uses, so `mount`/
// `umount` calls behave the way kubelet already expects (idempotency via IsMountPoint, etc).
package mount

import (
	"fmt"
	"os"

	mountutils "k8s.io/mount-utils"
)

// Mounter performs the staging (davfs2) and publish (bind) mounts for one node plugin instance.
type Mounter struct {
	iface mountutils.Interface
}

// New returns a Mounter backed by the real `mount`/`umount` binaries on PATH.
func New() *Mounter {
	return &Mounter{iface: mountutils.New("")}
}

// isMounted reports whether target is already a mount point, treating "doesn't exist" as false
// rather than an error so callers can call this before creating the directory.
func (m *Mounter) isMounted(target string) (bool, error) {
	notMnt, err := mountutils.IsNotMountPoint(m.iface, target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return !notMnt, nil
}

// MountDavfs mounts url (e.g. http://127.0.0.1:8888/dataroom/<title>) onto target via davfs2.
// Idempotent: a no-op if target is already mounted (NodeStageVolume may be retried by kubelet).
func (m *Mounter) MountDavfs(url, target string) error {
	mounted, err := m.isMounted(target)
	if err != nil {
		return fmt.Errorf("checking mount state of %s: %w", target, err)
	}
	if mounted {
		return nil
	}

	if err := os.MkdirAll(target, 0750); err != nil {
		return fmt.Errorf("creating staging dir %s: %w", target, err)
	}

	// No davfs2 credentials/secrets file needed: --auth is deliberately off on the loopback
	// webdav server (see internal/webdavsvc) — the server and this mount both run inside the
	// same node-plugin container/netns, so loopback really is the trust boundary here.
	if err := m.iface.Mount(url, target, "davfs", nil); err != nil {
		return fmt.Errorf("mount -t davfs %s %s: %w", url, target, err)
	}

	return nil
}

// UnmountDavfs unmounts a staging path previously mounted by MountDavfs. A no-op if it isn't
// currently mounted (NodeUnstageVolume may be retried by kubelet).
func (m *Mounter) UnmountDavfs(target string) error {
	return m.unmount(target)
}

// BindMount bind-mounts source (a staging path) onto target (a pod's target path). Idempotent.
func (m *Mounter) BindMount(source, target string) error {
	mounted, err := m.isMounted(target)
	if err != nil {
		return fmt.Errorf("checking mount state of %s: %w", target, err)
	}
	if mounted {
		return nil
	}

	if err := os.MkdirAll(target, 0750); err != nil {
		return fmt.Errorf("creating target dir %s: %w", target, err)
	}

	if err := m.iface.Mount(source, target, "", []string{"bind"}); err != nil {
		return fmt.Errorf("bind mount %s %s: %w", source, target, err)
	}

	return nil
}

// Unpublish undoes BindMount.
func (m *Mounter) Unpublish(target string) error {
	return m.unmount(target)
}

func (m *Mounter) unmount(target string) error {
	mounted, err := m.isMounted(target)
	if err != nil {
		return fmt.Errorf("checking mount state of %s: %w", target, err)
	}
	if !mounted {
		return nil
	}

	return mountutils.CleanupMountPoint(target, m.iface, true)
}
