package sandlock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	processPollInterval = 20 * time.Millisecond
	terminateGrace      = 200 * time.Millisecond
	confirmTimeout      = 2 * time.Second
)

// inContainerSandlockPath is where the host sandlock binary is copied inside
// the materialized rootfs (see rootfs.go/runtime.go), so it is already an
// ordinary file by the time this shell pivots into that tree. Bind-mounting
// the binary in at exec time instead was tried first and proved fragile: the
// mount did not reliably survive pivot_root with usable exec permission in
// practice, even with a recursive (--rbind) self-mount.
const inContainerSandlockPath = "/.aries-sandlock"

// pivotShell remaps the sandboxed process's mount namespace root to the
// materialized task rootfs before invoking sandlock, using the same
// unprivileged "rootless container" trick podman/bubblewrap rely on
// (unshare --user --map-root-user maps the caller's UID to root inside a
// fresh, isolated user namespace only, sufficient for pivot_root, granting no
// privilege on the host). taskdir is bind-mounted onto workdir first so the
// task's own writes land in its private per-task directory rather than the
// shared, read-only cached rootfs other tasks may be reading concurrently.
// If sandlock is dynamically linked (the common case: sandlock has no static
// build as of v0.8.6), interpreter is the in-container path of its embedded
// ELF interpreter (see runtime.go) and is invoked explicitly with
// --library-path so sandlock runs regardless of the task image's own libc
// (e.g. Alpine's musl, which lacks glibc entirely); interpreter is empty for
// a statically linked sandlock, which needs no such prefix.
const pivotShell = `set -e
rootfs=$1; workdir=$2; taskdir=$3; dir=$4; interpreter=$5; shift 5
mount --bind "$taskdir" "$rootfs$workdir"
mkdir -p "$rootfs/.aries-oldroot"
mount --bind "$rootfs" "$rootfs"
cd "$rootfs"
pivot_root . .aries-oldroot
umount -l /.aries-oldroot 2>/dev/null || true
cd "$dir"
if [ -n "$interpreter" ]; then
  exec "$interpreter" --library-path ` + inContainerLibDir + ` ` + inContainerSandlockPath + ` "$@"
else
  exec ` + inContainerSandlockPath + ` "$@"
fi`

// buildArgv assembles the unshare/pivot/sandlock invocation for one command.
// It is pure: it performs no I/O and can be tested without a sandlock binary.
// interpreterPath is the in-container path of sandlock's ELF interpreter
// (empty for a statically linked sandlock — see resolveSandlockRuntime).
func buildArgv(rootfsDir, workdir, taskDir, memoryFlag, interpreterPath string, command core.Command) ([]string, error) {
	dir := command.Dir
	if dir == "" {
		dir = workdir
	}
	if _, err := cleanSandboxPath(dir); err != nil {
		return nil, fmt.Errorf("invalid sandlock command dir: %w", err)
	}
	if _, err := cleanSandboxPath(command.Path); err != nil {
		return nil, fmt.Errorf("invalid sandlock command path: %w", err)
	}

	sandlockArgs := []string{"run", "-w", workdir, "-r", "/"}
	if memoryFlag != "" {
		sandlockArgs = append(sandlockArgs, "-m", memoryFlag)
	}
	sandlockArgs = append(sandlockArgs, "--", command.Path)
	sandlockArgs = append(sandlockArgs, command.Args...)

	argv := []string{
		"unshare", "--user", "--map-root-user", "--mount", "--pid", "--fork", "--",
		"/bin/sh", "-c", pivotShell, "aries-sandlock", rootfsDir, workdir, taskDir, dir, interpreterPath,
	}
	return append(argv, sandlockArgs...), nil
}

func memoryFlagFor(memoryMB int) string {
	if memoryMB <= 0 {
		return ""
	}
	return strconv.Itoa(memoryMB) + "M"
}

func buildEnv(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		result = append(result, key+"="+values[key])
	}
	return result
}

// runProcess starts argv as a new process group and waits for it, killing the
// whole group and positively confirming its absence if ctx ends before the
// process exits on its own. A nonzero exit is returned as a result, not a
// transport error, matching pkg/sandbox/docker's Exec contract. onStart, if
// non-nil, receives the process group ID as soon as the process starts, so
// the caller can track it for out-of-band cancellation (e.g. Stop).
func runProcess(ctx context.Context, argv []string, env []string, stdin io.Reader, stdout, stderr io.Writer, onStart func(pgid int)) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if len(argv) == 0 {
		return failure(), errors.New("sandlock process requires a non-empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return failure(), fmt.Errorf("start sandlock process: %w", err)
	}
	pgid := cmd.Process.Pid
	if onStart != nil {
		onStart(pgid)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case waitErr := <-waitDone:
		return processResult(waitErr, started), nil
	case <-ctx.Done():
		terminateErr := terminateProcessGroup(pgid)
		<-waitDone
		confirmErr := confirmProcessGroupAbsent(pgid)
		return failure(), errors.Join(ctx.Err(), terminateErr, confirmErr)
	}
}

func processResult(waitErr error, started time.Time) core.CommandResult {
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return core.CommandResult{ExitCode: exitCode, Duration: time.Since(started)}
}

func terminateProcessGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal sandlock process group: %w", err)
	}
	time.Sleep(terminateGrace)
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("force-kill sandlock process group: %w", err)
	}
	return nil
}

func confirmProcessGroupAbsent(pgid int) error {
	deadline := time.Now().Add(confirmTimeout)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("confirm sandlock process group %d absence: still present", pgid)
		}
		time.Sleep(processPollInterval)
	}
}
