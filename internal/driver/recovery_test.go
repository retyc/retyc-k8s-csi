package driver

import (
	"os"
	"path/filepath"
	"testing"

	mountutils "k8s.io/mount-utils"
)

func TestStageStore_SaveListRemove(t *testing.T) {
	store := &StageStore{Dir: filepath.Join(t.TempDir(), "staged")}
	tenant := &stageRecord{StagingPath: "/a/globalmount", Title: "pvc-a", Namespace: "team-a", Token: "t", Passphrase: "p"}
	if err := store.Save(tenant); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&stageRecord{StagingPath: "/b/globalmount", Title: "pvc-b"}); err != nil {
		t.Fatal(err)
	}
	// Saving again (a retried NodeStageVolume) replaces the record.
	if err := store.Save(tenant); err != nil {
		t.Fatal(err)
	}

	records, err := store.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("List() = %v, %v; want 2 records", records, err)
	}
	for _, rec := range records {
		creds := rec.credentials()
		switch rec.StagingPath {
		case "/a/globalmount":
			if creds == nil || creds.Token != "t" || creds.Passphrase != "p" || rec.Namespace != "team-a" {
				t.Errorf("tenant record = %+v", rec)
			}
		case "/b/globalmount":
			if creds != nil {
				t.Errorf("default identity record has credentials: %+v", rec)
			}
		}
	}

	// A record holds a tenant's credentials: owner-only.
	entries, _ := os.ReadDir(store.Dir)
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}

	if err := store.Remove("/a/globalmount"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("/a/globalmount"); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
	if records, _ := store.List(); len(records) != 1 || records[0].StagingPath != "/b/globalmount" {
		t.Fatalf("after Remove, List() = %v", records)
	}
}

func TestStageStore_SkipsCorruptedRecords(t *testing.T) {
	store := &StageStore{Dir: t.TempDir()}
	if err := store.Save(&stageRecord{StagingPath: "/a/globalmount", Title: "pvc-a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err == nil || len(records) != 1 {
		t.Fatalf("List() = %v, %v; want the valid record and an error", records, err)
	}
}

func TestStageStore_MissingDir(t *testing.T) {
	records, err := (&StageStore{Dir: filepath.Join(t.TempDir(), "none")}).List()
	if err != nil || records != nil {
		t.Fatalf("List() = %v, %v", records, err)
	}
}

func TestDeadTargets(t *testing.T) {
	const staging = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.retyc.com/abc/globalmount"
	const src = "http://127.0.0.1:8888/dataroom/pvc-1"
	podA := "/var/lib/kubelet/pods/a/volumes/kubernetes.io~csi/pvc-1/mount"
	podB := "/var/lib/kubelet/pods/b/volumes/kubernetes.io~csi/pvc-1/mount"
	podC := "/var/lib/kubelet/pods/c/volumes/kubernetes.io~csi/pvc-2/mount"
	rec := &stageRecord{StagingPath: staging, Title: "pvc-1"}
	infos := []mountutils.MountInfo{
		{MountPoint: "/", Major: 8, Minor: 1, Root: "/", Source: "/dev/sda1"},
		{MountPoint: staging, Major: 0, Minor: 50, Root: "/", Source: src},
		{MountPoint: podA, Major: 0, Minor: 50, Root: "/", Source: src, MountOptions: []string{"rw"}},
		// Recovered once already: an older dead layer, then the current one on top.
		{MountPoint: podB, Major: 0, Minor: 40, Root: "/", Source: src},
		{MountPoint: podB, Major: 0, Minor: 50, Root: "/", Source: src, MountOptions: []string{"ro"}},
		// Another volume that got the device number of an older mount of pvc-1.
		{MountPoint: podC, Major: 0, Minor: 40, Root: "/", Source: "http://127.0.0.1:8888/dataroom/pvc-2"},
		// A subdirectory of the same filesystem is not a NodePublishVolume bind mount.
		{MountPoint: "/elsewhere", Major: 0, Minor: 50, Root: "/sub", Source: src},
	}

	if top, ok := topMount(infos, podB); !ok || device(top) != "0:50" {
		t.Fatalf("topMount(podB) = %v, %v", top, ok)
	}
	if _, ok := topMount(infos, "/not/mounted"); ok {
		t.Fatal("topMount() reports an unmounted path as mounted")
	}

	targets := deadTargets(infos, rec, map[string]bool{"0:50": true})
	if len(targets) != 2 || targets[0].MountPoint != podA || targets[1].MountPoint != podB {
		t.Fatalf("deadTargets(0:50) = %v; want %s and %s", targets, podA, podB)
	}
	// podB's top layer is not 0:40, and podC's 0:40 is another dataroom.
	if targets := deadTargets(infos, rec, map[string]bool{"0:40": true}); len(targets) != 0 {
		t.Fatalf("deadTargets(0:40) = %v; want none", targets)
	}
	if targets := deadTargets(infos, rec, nil); len(targets) != 0 {
		t.Fatalf("deadTargets(nil) = %v; want none", targets)
	}
}
