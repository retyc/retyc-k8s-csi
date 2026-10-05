package driver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
)

// procMountInfo is read from the node plugin's own mount namespace, which sees kubelet's
// staging and pod directories through their Bidirectional mounts.
const procMountInfo = "/proc/self/mountinfo"

// RecoverInterval is how often Watch checks the staged volumes.
const RecoverInterval = 30 * time.Second

// Recover remounts the staged volumes whose davfs2 mount is dead. The davfs2 daemons live in the
// node plugin container and die with it, leaving every staged volume as a mount that answers
// ENOTCONN, and kubelet will not call NodeStageVolume or NodePublishVolume again for them. For
// each recorded staging path still mounted, Recover takes a reference on its identity's WebDAV
// server (so NodeUnstageVolume releases it as usual) and, if the mount is dead, mounts the
// dataroom again and stacks a bind mount of it on every pod target path that pointed at the dead
// one. Pods mounting the volume with mountPropagation HostToContainer see it come back; the others
// do at their next container restart. Run it once before serving CSI requests, then from Watch.
func (s *NodeServer) Recover(ctx context.Context) {
	if s.Stages == nil {
		return
	}
	records, err := s.Stages.List()
	if err != nil {
		klog.Warningf("recovery: reading stage records: %v", err)
	}

	var wg sync.WaitGroup
	for _, rec := range records {
		wg.Go(func() {
			defer s.locks.Lock(rec.StagingPath)()
			if err := s.recoverVolume(ctx, rec); err != nil {
				klog.Errorf("recovery: %s: %v", rec.StagingPath, err)
			}
		})
	}
	wg.Wait()
}

// Watch runs Recover every interval until ctx is done: a davfs2 daemon can also die on its own
// (OOM kill) while the node plugin keeps running.
func (s *NodeServer) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Recover(ctx)
		}
	}
}

// errNoStageRecord is returned by repairStaging for a dead staging mount the driver has no record
// of (staged by a driver version that did not record them).
var errNoStageRecord = errors.New("no stage record to remount it from; reschedule the pods using " +
	"this volume on this node")

// repairStaging remounts stagingPath if its davfs2 mount is dead or gone (locked per staging
// path). kubelet only publishes a volume it staged, so an unmounted staging path is one a failed
// recovery left behind.
func (s *NodeServer) repairStaging(ctx context.Context, stagingPath string) error {
	healthy := func() (bool, error) {
		mounted, corrupted, err := s.Mounter.State(stagingPath)

		return mounted && !corrupted, err
	}
	if ok, err := healthy(); err != nil || ok {
		return err
	}
	defer s.locks.Lock(stagingPath)()
	ok, err := healthy()
	if err != nil || ok {
		return err // repaired by a concurrent call
	}

	var rec *stageRecord
	if s.Stages != nil {
		if rec, err = s.Stages.Get(stagingPath); err != nil {
			return fmt.Errorf("staging mount %s is down: %w", stagingPath, err)
		}
	}
	if rec == nil {
		return fmt.Errorf("staging mount %s is down: %w", stagingPath, errNoStageRecord)
	}

	if err := s.recoverVolume(ctx, rec); err != nil {
		return err
	}
	if ok, err = healthy(); err != nil || !ok {
		return fmt.Errorf("staging mount %s is still down (%v): %w", stagingPath, err, errNoStageRecord)
	}

	return nil
}

// recoverVolume remounts rec's staging path if its davfs2 mount is dead or gone, and stacks a
// bind mount of the new one on every pod target path still bound to a dead one (the caller holds
// the staging path's lock).
func (s *NodeServer) recoverVolume(ctx context.Context, rec *stageRecord) error {
	infos, err := mountutils.ParseMountInfo(procMountInfo)
	if err != nil {
		return err
	}
	staging, mounted := topMount(infos, rec.StagingPath)
	corrupted := false
	if mounted {
		if _, corrupted, err = s.Mounter.State(rec.StagingPath); err != nil {
			return err
		}
	}
	// The filesystems pods may still be bound to and that no longer answer: the one recorded at the
	// last (re)mount, unless it is the healthy staging mount, and the staging mount if it is dead.
	dead := map[string]bool{}
	if rec.Device != "" && (!mounted || corrupted || device(staging) != rec.Device) {
		dead[rec.Device] = true
	}
	if corrupted {
		dead[device(staging)] = true
	}
	targets := deadTargets(infos, rec, dead)
	if !mounted && len(targets) == 0 {
		// Unstaged behind our back or a node reboot: kubelet stages it again if a pod needs it.
		klog.Infof("recovery: %s is no longer mounted, dropping its record", rec.StagingPath)

		return s.Stages.Remove(rec.StagingPath)
	}

	// The server reference stays held on failure: NodeUnstageVolume releases it.
	server, err := s.Webdav.Acquire(rec.StagingPath, rec.credentials(), rec.Namespace)
	if err != nil {
		return err
	}
	if mounted && !corrupted && len(targets) == 0 {
		return nil
	}

	if !mounted || corrupted {
		readyCtx, cancel := context.WithTimeout(ctx, webdavReadyTimeout)
		defer cancel()
		if err := server.WaitReady(readyCtx); err != nil {
			return fmt.Errorf("local webdav server not ready: %w", err)
		}
		source := dataroomURL(server, rec.Title)
		klog.Infof("recovery: remounting %s at %s", source, rec.StagingPath)
		if corrupted {
			if err := s.Mounter.Detach(rec.StagingPath); err != nil {
				return err
			}
		}
		if err := s.Mounter.MountDavfs(source, rec.StagingPath); err != nil {
			return err
		}
	}

	var errs []error
	for _, t := range targets {
		klog.Infof("recovery: rebinding %s", t.MountPoint)
		errs = append(errs, s.Mounter.Rebind(rec.StagingPath, t.MountPoint, slices.Contains(t.MountOptions, "ro")))
	}
	if err := errors.Join(errs...); err != nil {
		return err // the record keeps the dead device: the next round retries the targets
	}

	if infos, err = mountutils.ParseMountInfo(procMountInfo); err != nil {
		return err
	}
	if staging, mounted = topMount(infos, rec.StagingPath); mounted && device(staging) != rec.Device {
		rec.Device = device(staging)

		return s.Stages.Save(rec)
	}

	return nil
}

// device formats a mount's device as major:minor.
func device(info mountutils.MountInfo) string {
	return fmt.Sprintf("%d:%d", info.Major, info.Minor)
}

// topMount returns the topmost mount on path. mountinfo lists mounts parent first, so the last
// entry for a mount point is its top layer.
func topMount(infos []mountutils.MountInfo, path string) (top mountutils.MountInfo, mounted bool) {
	for _, info := range infos {
		if info.MountPoint == path {
			top, mounted = info, true
		}
	}

	return top, mounted
}

// deadTargets returns the mount points, other than the staging path, whose top layer is a bind
// mount of rec's dataroom from one of the dead devices: the pod target paths NodePublishVolume
// bound to a davfs2 mount that no longer answers. The source check guards against a device
// number reused by another volume's mount.
func deadTargets(infos []mountutils.MountInfo, rec *stageRecord, dead map[string]bool) []mountutils.MountInfo {
	if len(dead) == 0 {
		return nil
	}
	suffix := "/dataroom/" + url.PathEscape(rec.Title)
	var order []string
	top := map[string]mountutils.MountInfo{}
	for _, info := range infos {
		if info.MountPoint == rec.StagingPath {
			continue
		}
		if _, seen := top[info.MountPoint]; !seen {
			order = append(order, info.MountPoint)
		}
		top[info.MountPoint] = info
	}
	var targets []mountutils.MountInfo
	for _, mp := range order {
		info := top[mp]
		if dead[device(info)] && info.Root == "/" && strings.HasSuffix(info.Source, suffix) {
			targets = append(targets, info)
		}
	}

	return targets
}
