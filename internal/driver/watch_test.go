package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// ready makes the mount pod run, mount the volume and pass its readiness probe, and returns the
// device of the mount.
func (e *nodeEnv) ready() string {
	e.t.Helper()
	dev := e.mnt.fuse(e.staging, testVolume)
	pod := e.pod()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning, PodIP: "10.0.0.7",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "mount", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}

	return dev
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The pod watch repairs a volume without waiting for the recovery's round: a killed mount pod is
// replaced once its restart delay is over, and the target is bound again once the new one is ready.
func TestWatchRepairsAtOnce(t *testing.T) {
	e := newNodeEnvWith(t, func(c *mountpod.Config) { c.RestartBackoff = 50 * time.Millisecond })
	e.s.Pods.Now = time.Now // the queue's delays are real ones
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stage(e.stageRequest(nil))
	dev := e.ready()
	if c := e.stage(e.stageRequest(nil)); c != codes.OK {
		t.Fatalf("stage = %v", c)
	}
	e.publish(e.target)
	old := e.pod().UID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newRepairQueue()
	defer queue.ShutDown()
	if err := e.s.watchPods(ctx, queue); err != nil {
		t.Fatal(err)
	}
	go e.s.repairWorker(ctx, queue)

	// kill -9: the container exits 137, the mount is dead.
	e.mnt.kill(dev)
	e.exit(137, "")
	eventually(t, "a new mount pod", func() bool {
		p, err := e.kube.CoreV1().Pods(testNS).Get(ctx, mountpod.Name(testNode, testVolume), metav1.GetOptions{})

		return err == nil && p.UID != old && p.DeletionTimestamp == nil
	})
	newDev := e.ready()
	eventually(t, "the target bound to the new mount", func() bool {
		top, ok := e.mnt.top(e.target)

		return ok && device(top) == newDev
	})
}

// A repair that fails (the API refusing to release an exited pod whose restart time is over) is
// retried with a growing delay, not at once in a loop.
func TestWatchRetryRateLimited(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	e.exit(137, "")
	e.s.Recover(context.Background()) // counts the exit
	e.advance(time.Hour)              // the restart time is long over
	deletes := 0
	e.kube.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		deletes++

		return true, nil, errors.New("connection refused")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newRepairQueue()
	defer queue.ShutDown()
	go e.s.repairWorker(ctx, queue)
	queue.Add(testVolume)
	eventually(t, "a first repair", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()

		return deletes > 0
	})
	time.Sleep(minRetryDelay + minRetryDelay/2)
	e.mu.Lock()
	defer e.mu.Unlock()
	// 0 s, 1 s, then 3 s: two tries in 1.5 s, three with a slow first one.
	if deletes < 2 || deletes > 3 {
		t.Fatalf("%d tries in %s, want 2 or 3", deletes, minRetryDelay+minRetryDelay/2)
	}
}
