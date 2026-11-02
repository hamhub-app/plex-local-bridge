package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"

	"github.com/hamhub-app/plex-local-bridge/internal/config"
)

func TestStopDoesNotSignalUnrelatedProcess(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "supervisor.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunningPID(state); err == nil {
		t.Fatal("unrelated process accepted as supervisor")
	}
	if err := Stop(state); err != nil {
		t.Fatal(err)
	}
	if err := StopCommand(state, "supervisor.pid", "daemon"); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("stopped unrelated process:", err)
	}
}

func TestManagedProcessLockAndPIDCleanup(t *testing.T) {
	state := t.TempDir()
	release, err := claimProcess(state, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := claimProcess(state, "daemon"); err == nil {
		t.Fatal("second daemon acquired lock")
	}
	if _, err := os.Stat(filepath.Join(state, "daemon.pid")); err != nil {
		t.Fatal("failed startup removed active process PID")
	}
	release()
	if _, err := os.Stat(filepath.Join(state, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatal("PID remains after shutdown")
	}
}

func TestInvalidPID(t *testing.T) {
	state := t.TempDir()
	for _, value := range []string{"", "1", "-1", "not-a-number", "999999999999999999999"} {
		os.WriteFile(filepath.Join(state, "supervisor.pid"), []byte(value), 0600)
		if _, err := RunningPID(state); err == nil {
			t.Fatalf("accepted PID %q", value)
		}
	}
}

func TestSupervisorRequiresLinuxRoot(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("root lifecycle covered by Docker integration")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	state := filepath.Join(t.TempDir(), "not-created")
	if err := Run(context.Background(), config.Default(), state, "root", logger); err == nil {
		t.Fatal("started privileged supervisor")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("rejected startup modified state directory")
	}
}
