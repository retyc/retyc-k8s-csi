package mount

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(self) = false")
	}

	// A child that exited and was not waited for is a zombie.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	pid := cmd.Process.Pid
	for range 100 {
		if !processAlive(pid) {
			break
		}
		_ = exec.Command("sleep", "0.01").Run()
	}
	if processAlive(pid) {
		t.Fatalf("processAlive(zombie %d) = true", pid)
	}
	_ = cmd.Wait()
	if processAlive(pid) {
		t.Fatalf("processAlive(reaped %d) = true", pid)
	}
}
