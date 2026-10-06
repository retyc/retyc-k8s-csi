package mount

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Detach lazily unmounts target (MNT_DETACH): a dead davfs2 mount still referenced by a pod's
// bind mount or an open file would refuse a plain unmount with EBUSY.
func (m *Mounter) Detach(target string) error {
	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("detaching %s: %w", target, err)
	}

	return nil
}

// Rebind bind-mounts source on top of whatever is mounted on target, without unmounting it
// first. That is what lets a running pod see a recovered volume: its container's mount of the
// volume is a copy of target's mount, and with mountPropagation HostToContainer a mount stacked
// on target propagates into that copy, whereas unmounting target and mounting it anew would not.
// The raw syscall, not `mount`: util-linux stats target first, which a dead FUSE mount answers
// with ENOTCONN. The layers left below are removed by Unpublish.
func (m *Mounter) Rebind(source, target string, readonly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount %s %s: %w", source, target, err)
	}
	if !readonly {
		return nil
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remounting %s read-only: %w", target, err)
	}

	return nil
}
