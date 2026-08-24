package sandlock

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestBuildArgv(t *testing.T) {
	command := core.Command{Path: "/usr/bin/python3", Args: []string{"-c", "print(1)"}}
	argv, err := buildArgv("/usr/local/bin/sandlock", "/cache/rootfs/abc", "/testbed", "/runs/task1/sandlock-work", "512M", command)
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"unshare", "--user", "--map-root-user", "--mount", "--pid", "pivot_root",
		"/usr/local/bin/sandlock", "-w", "/testbed", "-m", "512M", "/usr/bin/python3", "print(1)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("buildArgv output missing %q\nargv: %v", want, argv)
		}
	}
}

func TestBuildArgvRejectsInvalidPath(t *testing.T) {
	command := core.Command{Path: "relative-binary"}
	if _, err := buildArgv("/bin/sandlock", "/rootfs", "/testbed", "/work", "", command); err == nil {
		t.Error("buildArgv should reject a non-absolute command path")
	}
}

func TestBuildArgvOmitsMemoryFlagWhenUnset(t *testing.T) {
	command := core.Command{Path: "/bin/true"}
	argv, err := buildArgv("/bin/sandlock", "/rootfs", "/testbed", "/work", "", command)
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	for _, token := range argv {
		if token == "-m" {
			t.Errorf("buildArgv should omit a standalone -m token when memoryFlag is empty: %v", argv)
		}
	}
}

func TestMemoryFlagFor(t *testing.T) {
	if got := memoryFlagFor(0); got != "" {
		t.Errorf("memoryFlagFor(0) = %q, want empty", got)
	}
	if got := memoryFlagFor(512); got != "512M" {
		t.Errorf("memoryFlagFor(512) = %q, want 512M", got)
	}
}

func TestBuildEnv(t *testing.T) {
	env := buildEnv(map[string]string{"B": "2", "A": "1"})
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Errorf("buildEnv = %v, want sorted [A=1 B=2]", env)
	}
}

// runProcess is exercised directly against real host binaries here (not the
// sandlock/unshare argv, which needs a real target host to run) so its
// process-group start/wait/cancel logic is genuinely verified in this
// environment.

func TestRunProcessCapturesExitCodeAndOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	result, err := runProcess(context.Background(),
		[]string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 7"},
		nil, nil, &stdout, &stderr, nil)
	if err != nil {
		t.Fatalf("runProcess: %v", err)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
	if stdout.String() != "out\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "out\n")
	}
	if stderr.String() != "err\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "err\n")
	}
}

func TestRunProcessOnStartReceivesPGID(t *testing.T) {
	var pgid int
	_, err := runProcess(context.Background(), []string{"/bin/true"}, nil, nil, nil, nil, func(pid int) { pgid = pid })
	if err != nil {
		t.Fatalf("runProcess: %v", err)
	}
	if pgid == 0 {
		t.Error("onStart was not called with a nonzero pgid")
	}
}

func TestRunProcessCancellationTerminatesGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var pgid int
	done := make(chan struct{})
	var result core.CommandResult
	var runErr error
	go func() {
		result, runErr = runProcess(ctx,
			[]string{"/bin/sh", "-c", "trap '' TERM; sleep 30 & wait"},
			nil, nil, nil, nil, func(pid int) { pgid = pid })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runProcess did not return after cancellation")
	}
	if runErr == nil {
		t.Error("expected a cancellation error")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on cancellation", result.ExitCode)
	}
	if pgid == 0 {
		t.Fatal("pgid was never recorded")
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process group %d should be gone after cancellation, kill(0) = %v", pgid, err)
	}
}
