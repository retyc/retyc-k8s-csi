package mount

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	mountutils "k8s.io/mount-utils"
)

// layered is a mount table where several mounts stack on one path, as the recovery leaves them on a
// pod's target path; refuse makes umount fail on a layer, with umount's output.
type layered struct {
	*mountutils.FakeMounter
	layers  map[string][]string // path → devices, bottom first
	refuse  func(path, device string) error
	detachs int
}

func (l *layered) IsMountPoint(file string) (bool, error) {
	if _, err := os.Stat(file); err != nil {
		return false, err
	}

	return len(l.layers[file]) > 0, nil
}

func (l *layered) pop(target string) {
	l.layers[target] = l.layers[target][:len(l.layers[target])-1]
}

func (l *layered) Unmount(target string) error {
	stack := l.layers[target]
	if len(stack) == 0 {
		return errors.New("unmount failed: exit status 32\nUnmounting arguments: " + target +
			"\nOutput: umount: " + target + ": not mounted.\n")
	}
	if l.refuse != nil {
		if err := l.refuse(target, stack[len(stack)-1]); err != nil {
			return err
		}
	}
	l.pop(target)

	return nil
}

func newLayered(t *testing.T, devices ...string) (*Mounter, *layered, string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	l := &layered{FakeMounter: mountutils.NewFakeMounter(nil), layers: map[string][]string{target: devices}}
	m := &Mounter{iface: l, detach: func(p string) error {
		l.detachs++
		l.pop(p)

		return nil
	}}

	return m, l, target
}

// Every layer goes, the directory with them; a layer umount refuses is detached.
func TestUnpublishStackedLayers(t *testing.T) {
	m, l, target := newLayered(t, "dead-1", "dead-2", "alive")
	l.refuse = func(_, device string) error {
		if device == "dead-1" {
			return errors.New("unmount failed: exit status 32\nUnmounting arguments: " + target +
				"\nOutput: umount: " + target + ": target is busy.\n")
		}

		return nil
	}
	if err := m.Unpublish(target); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if len(l.layers[target]) != 0 || l.detachs != 1 {
		t.Fatalf("layers left %v, %d detaches; want none, 1", l.layers[target], l.detachs)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target directory left: %v", err)
	}
	// Idempotent.
	if err := m.Unpublish(target); err != nil {
		t.Fatalf("Unpublish again: %v", err)
	}
}

// umount's "not mounted" (a layer gone meanwhile) is no failure.
func TestUnpublishNotMounted(t *testing.T) {
	m, l, target := newLayered(t, "alive")
	l.refuse = func(p, _ string) error {
		l.pop(p) // gone under our feet

		return errors.New("unmount failed: exit status 32\nOutput: umount: " + p + ": not mounted.\n")
	}
	if err := m.Unpublish(target); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if l.detachs != 0 {
		t.Fatalf("%d detaches, want none", l.detachs)
	}
}

// umount(8) says "not mounted" of a layer that still is (its view of the mount table is not the
// kernel's): the layer is detached instead of retrying the same umount.
func TestUnpublishNotMountedButMounted(t *testing.T) {
	m, l, target := newLayered(t, "dead-1", "dead-2")
	unmounts := 0
	l.refuse = func(p, _ string) error {
		unmounts++

		return errors.New("unmount failed: exit status 32\nOutput: umount: " + p + ": not mounted.\n")
	}
	if err := m.Unpublish(target); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if len(l.layers[target]) != 0 || l.detachs != 2 || unmounts != 2 {
		t.Fatalf("layers left %v, %d detaches, %d umounts; want none, 2, 2", l.layers[target], l.detachs, unmounts)
	}
}

// A mount that never goes: an error once the bound is reached, not an endless loop.
func TestUnpublishBounded(t *testing.T) {
	m, l, target := newLayered(t, slices.Repeat([]string{"dead"}, maxStackedMounts+1)...)
	if err := m.Unpublish(target); err == nil {
		t.Fatalf("Unpublish of %d layers succeeded", len(l.layers[target])+maxStackedMounts)
	}
	m.detach = func(string) error { return errors.New("detaching: EPERM") }
	l.refuse = func(string, string) error { return errors.New("unmount failed: exit status 32") }
	if err := m.Unpublish(target); err == nil {
		t.Fatal("Unpublish succeeded although neither umount nor the detach worked")
	}
}
