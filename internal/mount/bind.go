// Package mount wraps the mount operations the node plugin needs: bind-mounting the staging path
// (a retycfs FUSE mount owned by a mount pod) into a pod's target path at publish time, and
// repairing those mounts. Built on k8s.io/mount-utils, the same library every CSI node plugin uses, so `mount`/
// `umount` calls behave the way kubelet already expects (idempotency via IsMountPoint, etc).
package mount

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
)

// Mounter performs the publish (bind) mounts for one node plugin instance.
type Mounter struct {
	iface mountutils.Interface
	// detach replaces Detach in unmount (tests); nil means Detach.
	detach func(target string) error
}

// New returns a Mounter backed by the real `mount`/`umount` binaries on PATH.
func New() *Mounter {
	return &Mounter{iface: mountutils.New("")}
}

// mountState reports whether target is a mount point. corrupted is true when the path is a
// mount whose backing daemon is gone (FUSE daemon killed → "Transport endpoint is not connected"),
// which stat() reports as an error rather than "not mounted".
func (m *Mounter) mountState(target string) (mounted, corrupted bool, err error) {
	isMnt, err := m.iface.IsMountPoint(target)
	switch {
	case err == nil:
		return isMnt, false, nil
	case os.IsNotExist(err):
		return false, false, nil
	case mountutils.IsCorruptedMnt(err):
		return true, true, nil
	default:
		return false, false, err
	}
}

// BindMount bind-mounts source (a staging path) onto target (a pod's target path), read-only
// when readonly is set. Idempotent.
func (m *Mounter) BindMount(source, target string, readonly bool) error {
	mounted, corrupted, err := m.mountState(target)
	if err != nil {
		return fmt.Errorf("checking mount state of %s: %w", target, err)
	}
	if corrupted {
		if err := m.iface.Unmount(target); err != nil {
			return fmt.Errorf("unmounting corrupted mount %s: %w", target, err)
		}
		mounted = false
	}
	if mounted {
		return nil
	}

	if err := os.MkdirAll(target, 0750); err != nil {
		return fmt.Errorf("creating target dir %s: %w", target, err)
	}

	// mount-utils handles "bind" specially: it bind-mounts first, then remounts with the
	// remaining options (here "ro"), which is the only way to get a read-only bind mount.
	options := []string{"bind"}
	if readonly {
		options = append(options, "ro")
	}
	if err := m.iface.Mount(source, target, "", options); err != nil {
		return fmt.Errorf("bind mount %s %s: %w", source, target, err)
	}

	return nil
}

// Unpublish undoes BindMount.
func (m *Mounter) Unpublish(target string) error {
	return m.unmount(target)
}

// State reports whether target is a mount point, and whether its FUSE daemon is gone.
func (m *Mounter) State(target string) (mounted, corrupted bool, err error) {
	return m.mountState(target)
}

// Unmount unmounts the top layer of target, with a plain umount: enough for a dead FUSE mount.
func (m *Mounter) Unmount(target string) error {
	return m.iface.Unmount(target)
}

// maxStackedMounts bounds unmount's loop. The recovery stacks a fresh bind mount on top of a dead
// one each time the volume's mount pod is replaced, so a pod's target path can hold a few layers.
const maxStackedMounts = 32

// unmount unmounts every layer mounted on target (corrupted or not), until it is no longer a mount
// point, and removes the directory. A layer that umount refuses (busy, or a dead FUSE mount that
// umount(8) fails to stat) is detached instead. umount's "not mounted" counts as done when the
// layer did go (unmounted meanwhile); when target is still a mount point, umount(8) is wrong about
// it (it reads its own view of the mount table) and the layer is detached as well.
func (m *Mounter) unmount(target string) error {
	for i := range maxStackedMounts {
		mounted, _, err := m.mountState(target)
		if err != nil {
			return fmt.Errorf("checking mount state of %s: %w", target, err)
		}
		if !mounted {
			if i == 0 {
				return nil
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", target, err)
			}

			return nil
		}
		err = m.iface.Unmount(target) // the error holds umount's output
		switch {
		case err == nil:
		case strings.Contains(err.Error(), "not mounted"):
			klog.V(2).Infof("unmounting %s: %v", target, err)
			if still, _, serr := m.mountState(target); serr != nil || !still {
				break // gone meanwhile, or the next turn reports the error
			}
			klog.Warningf("unmounting layer %d of %s: umount says not mounted but it still is; detaching it", i+1,
				target)
			if err := m.lazyUnmount(target); err != nil {
				return err
			}
		default:
			klog.Warningf("unmounting layer %d of %s: %v; detaching it", i+1, target, err)
			if err := m.lazyUnmount(target); err != nil {
				return err
			}
		}
	}

	return fmt.Errorf("%s is still mounted after %d unmounts", target, maxStackedMounts)
}

func (m *Mounter) lazyUnmount(target string) error {
	if m.detach != nil {
		return m.detach(target)
	}

	return m.Detach(target)
}
