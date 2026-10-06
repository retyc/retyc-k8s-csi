// Package mount wraps the two mount operations the node plugin needs: mounting a dataroom via
// davfs2 at staging time, and bind-mounting the staging path into a pod's target path at publish
// time. Built on k8s.io/mount-utils, the same library every CSI node plugin uses, so `mount`/
// `umount` calls behave the way kubelet already expects (idempotency via IsMountPoint, etc).
package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
)

// Mounter performs the staging (davfs2) and publish (bind) mounts for one node plugin instance.
type Mounter struct {
	iface mountutils.Interface
	// davfsOptions are passed as `-o` to every `mount -t davfs`.
	davfsOptions []string
}

// DefaultDavfsOptions makes the mount usable by any pod UID. davfs2 defaults to root-owned
// 0700/0600 entries, and kubelet cannot apply fsGroup here (CSIDriver fsGroupPolicy=None — a
// chown/chmod on a FUSE/WebDAV mount is meaningless), so permissive mode bits are the only way
// non-root pods get access. Everything else (ask_auth, delay_upload, dir_refresh, ...) lives in
// /etc/davfs2/davfs2.conf, shipped by the Dockerfile.
var DefaultDavfsOptions = []string{"dir_mode=0777", "file_mode=0666"}

// New returns a Mounter backed by the real `mount`/`umount` binaries on PATH.
func New(davfsOptions []string) *Mounter {
	return &Mounter{iface: mountutils.New(""), davfsOptions: davfsOptions}
}

// mountState reports whether target is a mount point. corrupted is true when the path is a
// mount whose backing daemon is gone (davfs2 killed → "Transport endpoint is not connected"),
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

// MountDavfs mounts url (e.g. http://127.0.0.1:8888/dataroom/<title>) onto target via davfs2.
// Idempotent: a no-op if target is already mounted (NodeStageVolume may be retried by kubelet).
// A corrupted leftover mount (davfs2 daemon died, typically after a node-plugin pod restart) is
// torn down and re-mounted instead of failing forever.
func (m *Mounter) MountDavfs(url, target string) error {
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
		return fmt.Errorf("creating staging dir %s: %w", target, err)
	}
	removeStalePIDFile(target)

	// No davfs2 credentials: --auth is deliberately off on the loopback webdav server (see
	// internal/webdavsvc), and /etc/davfs2/davfs2.conf sets ask_auth=0 so mount.davfs never
	// prompts (it would otherwise block on a username prompt with no TTY and fail).
	if err := m.iface.Mount(url, target, "davfs", m.davfsOptions); err != nil {
		return fmt.Errorf("mount -t davfs %s %s: %w", url, target, err)
	}

	return nil
}

// UnmountDavfs unmounts a staging path previously mounted by MountDavfs. A no-op if it isn't
// currently mounted (NodeUnstageVolume may be retried by kubelet).
func (m *Mounter) UnmountDavfs(target string) error {
	return m.unmount(target)
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

// maxStackedMounts bounds unmount's loop. Recover stacks a fresh bind mount on top of a dead one
// at each node plugin restart, so a pod's target path can hold a few layers.
const maxStackedMounts = 16

// unmount unmounts every layer mounted on target (corrupted or not) and removes the directory.
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
		if err := m.iface.Unmount(target); err != nil {
			return err
		}
	}

	return fmt.Errorf("%s is still mounted after %d unmounts", target, maxStackedMounts)
}

// davfsPIDDir is where mount.davfs keeps one PID file per mount point (davfs2's default).
const davfsPIDDir = "/var/run/mount.davfs"

// removeStalePIDFile removes target's davfs2 PID file when its process is gone. mount.davfs
// refuses to mount over a PID file left by a daemon that was killed (OOM) instead of unmounted.
func removeStalePIDFile(target string) {
	name := filepath.Join(davfsPIDDir, strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "-")+".pid")
	data, err := os.ReadFile(name) //nolint:gosec // G304: a path under davfsPIDDir
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && processAlive(pid) {
		return
	}
	if err := os.Remove(name); err == nil {
		klog.Infof("removed the stale davfs2 PID file %s", name)
	}
}

// processAlive reports whether pid runs and is not a zombie: a daemon killed while its parent
// does not reap it (no init in the container) keeps its /proc entry.
func processAlive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// "pid (comm) state ...": comm may contain spaces and parentheses, the state follows the last ')'.
	i := strings.LastIndexByte(string(stat), ')')

	return i < 0 || i+2 >= len(stat) || stat[i+2] != 'Z'
}
