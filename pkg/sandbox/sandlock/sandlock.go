// Package sandlock implements a ToolSandbox backed by Sandlock
// (https://github.com/multikernel/sandlock), a Landlock+seccomp process
// sandbox, instead of a Docker container.
//
// Sandlock confines a process to literal host filesystem paths; it has no
// container/image concept of its own. Terminal-Bench 2 tasks are pinned
// Docker images, so each task's image filesystem is first materialized once
// (via Docker create+copy, never start — see rootfs.go) into a shared,
// read-only, digest-cached directory, and only the task's own work directory
// gets a fresh per-task writable copy, bind-mounted over the shared rootfs
// inside a private, unprivileged mount namespace before Sandlock runs (see
// process.go's pivotShell). This keeps concurrent tasks sharing one cached
// image from corrupting each other's writes without requiring host root.
//
// The existing SSH bridges (pkg/bridge/openclawssh, pkg/bridge/hermesssh)
// require a Docker-network-shaped sandbox capability (ContainerID,
// ContainerName, NetworkName, NetworkGateway, ExecStream — see bridge.go).
// This package satisfies that surface with a minimal Docker network created
// solely for SSH reachability between the harness container and this host
// process; the task itself never runs inside a container or is attached to
// that network. See bridge.go's networkClient doc comment for details.
package sandlock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	defaultDockerSocket   = "/var/run/docker.sock"
	defaultCleanupTimeout = 30 * time.Second
	maxExecOutput         = 16 << 20
)

var (
	_ runner.ToolSandbox = (*Manager)(nil)
	_ runner.Sandbox     = (*Sandbox)(nil)
)

// Options are the host-local inputs to the Sandlock sandbox manager.
type Options struct {
	OutputDir      string
	RootfsCacheDir string
	// Executable is the path to the sandlock CLI binary.
	Executable string
	// DockerSocket is used only to materialize a pinned image's filesystem;
	// no container built from it is ever started.
	DockerSocket   string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager starts one Sandlock-confined task process at a time per task,
// backed by a shared cache of materialized image filesystems.
type Manager struct {
	docker         dockerAPI
	closeClient    func() error
	outputDir      string
	rootfsCacheDir string
	executable     string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)

	mu        sync.Mutex
	sandboxes map[string]*Sandbox
	closeOnce sync.Once
	closeErr  error
}

// Sandbox is a live Sandlock task environment.
type Sandbox struct {
	owner          *Manager
	id             string
	rootfsDir      string
	workdir        string
	taskDir        string
	executable     string
	memoryFlag     string
	cleanupTimeout time.Duration
	runID          string
	taskID         string

	// networkName/networkOwned track the Docker network created solely for
	// SSH bridge reachability (see bridge.go); the task process is never
	// attached to it.
	networkName  string
	networkOwned bool

	mu         sync.Mutex
	activePGID int
	stopped    bool
	stopping   bool
	stopDone   chan struct{}
	stopErr    error
}

// New constructs a Sandlock manager without contacting the daemon.
func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("sandlock sandbox output directory is required")
	}
	if strings.TrimSpace(options.RootfsCacheDir) == "" {
		return nil, errors.New("sandlock sandbox rootfs cache directory is required")
	}
	if strings.TrimSpace(options.Executable) == "" {
		return nil, errors.New("sandlock sandbox executable path is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandlock sandbox output directory: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandlock sandbox output directory: %w", err)
	}
	rootfsCacheDir, err := filepath.Abs(options.RootfsCacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandlock rootfs cache directory: %w", err)
	}
	if err := os.MkdirAll(rootfsCacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandlock rootfs cache directory: %w", err)
	}
	socket := options.DockerSocket
	if socket == "" {
		socket = defaultDockerSocket
	}
	host := socket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-sandlock/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client for sandlock rootfs extraction: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		docker: api, closeClient: api.Close,
		outputDir: outputDir, rootfsCacheDir: rootfsCacheDir, executable: options.Executable,
		cleanupTimeout: options.CleanupTimeout, logger: options.Logger, newID: randomID,
		sandboxes: make(map[string]*Sandbox),
	}, nil
}

// Close releases the manager's Docker SDK transport used for rootfs
// materialization. Resource cleanup remains Stop's responsibility.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		if m.closeClient != nil {
			m.closeErr = m.closeClient()
		}
	})
	return m.closeErr
}

// Start materializes the requested image's filesystem (if not already
// cached), seeds a fresh per-task writable work directory from it, and
// returns a sandbox ready to run Sandlock-confined commands in that
// directory.
func (m *Manager) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if err := validateIdentity("run", request.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", request.TaskID); err != nil {
		return nil, err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return nil, err
	}
	workdir, err := requireNonRoot(request.Environment.Workdir)
	if err != nil {
		return nil, fmt.Errorf("invalid sandlock sandbox workdir: %w", err)
	}
	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("generate sandlock sandbox ID: %w", err)
	}

	rootfsDir, err := materializeRootfs(ctx, m.docker, m.rootfsCacheDir, request.Environment.Image, workdir)
	if err != nil {
		return nil, fmt.Errorf("materialize sandlock rootfs: %w", err)
	}

	taskDir := filepath.Join(m.outputDir, request.TaskID, "sandlock-work")
	if err := os.RemoveAll(taskDir); err != nil {
		return nil, fmt.Errorf("clear sandlock task work directory: %w", err)
	}
	if err := seedWorkdir(rootfsDir, workdir, taskDir); err != nil {
		return nil, fmt.Errorf("seed sandlock task work directory: %w", err)
	}

	sandbox := &Sandbox{
		owner: m, id: id, rootfsDir: rootfsDir, workdir: workdir, taskDir: taskDir,
		networkName: "aries-sandlock-net-" + id,
		executable:  m.executable, memoryFlag: memoryFlagFor(request.Environment.MemoryMB),
		cleanupTimeout: m.cleanupTimeout, runID: request.RunID, taskID: request.TaskID,
	}
	if err := createReachabilityNetwork(ctx, m.docker, request, sandbox.networkName); err != nil {
		os.RemoveAll(taskDir)
		return nil, fmt.Errorf("create sandlock bridge reachability network: %w", err)
	}
	sandbox.networkOwned = true

	m.mu.Lock()
	m.sandboxes[request.TaskID] = sandbox
	m.mu.Unlock()

	m.logger.WithContext(ctx).WithFields(logrus.Fields{"rootfs": rootfsDir, "task": request.TaskID, "network": sandbox.networkName}).Info("sandlock task sandbox started")
	return sandbox, nil
}

// Stop terminates any in-flight command and removes the task's writable work
// directory. The shared, digest-cached rootfs persists intentionally across
// tasks and is never removed here.
func (m *Manager) Stop(ctx context.Context, live runner.Sandbox) error {
	if live == nil {
		return errors.New("stop sandlock sandbox: sandbox is required")
	}
	sandbox, ok := live.(*Sandbox)
	if !ok || sandbox == nil {
		return fmt.Errorf("stop sandlock sandbox: unsupported sandbox type %T", live)
	}
	if sandbox.owner != m {
		return errors.New("stop sandlock sandbox: sandbox belongs to another manager")
	}
	err := sandbox.stop(ctx)
	m.mu.Lock()
	if m.sandboxes[sandbox.taskID] == sandbox {
		delete(m.sandboxes, sandbox.taskID)
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) activeProcesses(runID string) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]int, len(m.sandboxes))
	for _, sandbox := range m.sandboxes {
		if sandbox.runID != runID {
			continue
		}
		sandbox.mu.Lock()
		pgid := sandbox.activePGID
		sandbox.mu.Unlock()
		if pgid != 0 {
			result[sandbox.taskID] = pgid
		}
	}
	return result
}

// Exec runs one argv Sandlock-confined to the task's rootfs and work
// directory. Nonzero exits are returned as results, not transport errors. It
// is a buffering wrapper around ExecStream (see bridge.go), which the SSH
// bridges use directly for live streaming.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	var stdin io.Reader
	if len(command.Stdin) > 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	var stdout, stderr bytes.Buffer
	result, err := s.ExecStream(ctx, command, stdin, &stdout, &stderr)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, err
}

// Upload copies one regular host file into the task's writable work
// directory. Only paths inside the sandbox's work directory are accepted:
// everywhere else in the sandboxed filesystem is the shared, read-only
// cached rootfs, and writing there would corrupt other tasks using the same
// cached image.
func (s *Sandbox) Upload(ctx context.Context, source, destination string) error {
	target, err := s.taskPath(destination)
	if err != nil {
		return fmt.Errorf("invalid sandlock upload destination: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stat sandlock upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("sandlock upload source must be a regular file")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create sandlock upload destination directory: %w", err)
	}
	content, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open sandlock upload source: %w", err)
	}
	defer content.Close()
	return atomicWrite(target, content, info.Mode().Perm())
}

// Download copies one regular file from the task's writable work directory
// beneath the configured output directory, subject to the same work
// directory boundary as Upload.
func (s *Sandbox) Download(ctx context.Context, source, destination string) error {
	target, err := s.taskPath(source)
	if err != nil {
		return fmt.Errorf("invalid sandlock download source: %w", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("stat sandlock download source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("sandlock download source must be a regular file")
	}
	resolvedDestination, err := outputPath(s.owner.outputDir, destination)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedDestination), 0o700); err != nil {
		return fmt.Errorf("create sandlock download directory: %w", err)
	}
	content, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("open sandlock download source: %w", err)
	}
	defer content.Close()
	return atomicWrite(resolvedDestination, content, 0o600)
}

// taskPath resolves an absolute task-visible path to its host location
// inside the per-task writable directory, rejecting anything outside the
// sandbox's work directory.
func (s *Sandbox) taskPath(taskVisiblePath string) (string, error) {
	clean, err := cleanSandboxPath(taskVisiblePath)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(s.workdir, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the sandbox work directory %q", taskVisiblePath, s.workdir)
	}
	return filepath.Join(s.taskDir, relative), nil
}

func atomicWrite(target string, content io.Reader, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".aries-sandlock-*")
	if err != nil {
		return fmt.Errorf("create sandlock destination: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.Copy(temporary, content); err != nil {
		temporary.Close()
		return fmt.Errorf("write sandlock file: %w", err)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set sandlock file mode: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close sandlock file: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("publish sandlock file: %w", err)
	}
	return nil
}

// stop records termination of any in-flight command and removes the task
// work directory. Concurrent and repeated calls are safe.
func (s *Sandbox) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	if s.stopping {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.stopErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.stopping = true
	s.stopDone = make(chan struct{})
	done := s.stopDone
	s.mu.Unlock()

	err := s.stopOnce()
	s.mu.Lock()
	s.stopErr = err
	s.stopping = false
	s.stopped = err == nil
	close(done)
	s.mu.Unlock()
	return err
}

func (s *Sandbox) stopOnce() error {
	s.mu.Lock()
	pgid := s.activePGID
	s.mu.Unlock()
	var errs []error
	if pgid != 0 {
		if err := terminateProcessGroup(pgid); err != nil {
			errs = append(errs, fmt.Errorf("terminate sandlock task process: %w", err))
		}
		if err := confirmProcessGroupAbsent(pgid); err != nil {
			errs = append(errs, fmt.Errorf("confirm sandlock task process absence: %w", err))
		} else {
			s.mu.Lock()
			s.activePGID = 0
			s.mu.Unlock()
		}
	}
	if err := os.RemoveAll(s.taskDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandlock task work directory: %w", err))
	}
	if _, err := os.Stat(s.taskDir); err == nil {
		errs = append(errs, errors.New("confirm sandlock task work directory absence: directory still exists"))
	} else if !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("confirm sandlock task work directory absence: %w", err))
	}
	if s.networkOwned {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.cleanupTimeout)
		if err := removeReachabilityNetwork(cleanupCtx, s.owner.docker, s.networkName); err != nil {
			errs = append(errs, err)
		} else {
			s.networkOwned = false
		}
		cancel()
	}
	return errors.Join(errs...)
}

func validateEnvironment(environment core.Environment) error {
	if err := validatePinnedImage(environment.Image); err != nil {
		return fmt.Errorf("invalid sandlock sandbox image: %w", err)
	}
	if _, err := requireNonRoot(environment.Workdir); err != nil {
		return fmt.Errorf("invalid sandlock sandbox workdir: %w", err)
	}
	if environment.AllowNetwork {
		return errors.New("sandlock sandbox does not support allow_network: true; Sandlock requires an explicit per-host allow-list that ARIES's boolean allow_network cannot express")
	}
	if environment.MemoryMB < 0 {
		return errors.New("sandlock sandbox memory must be nonnegative")
	}
	if environment.StorageMB < 0 {
		return errors.New("sandlock sandbox storage must be nonnegative")
	}
	if environment.GPUs != 0 {
		return errors.New("sandlock sandbox does not support GPU devices")
	}
	for key, value := range environment.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid sandlock sandbox environment %q", key)
		}
	}
	return nil
}

func validateCommand(command core.Command) error {
	if _, err := cleanSandboxPath(command.Path); err != nil {
		return fmt.Errorf("invalid command path: %w", err)
	}
	if command.Dir != "" {
		if _, err := cleanSandboxPath(command.Dir); err != nil {
			return fmt.Errorf("invalid command workdir: %w", err)
		}
	}
	if command.Timeout < 0 {
		return errors.New("command timeout must be nonnegative")
	}
	for _, argument := range command.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("command argument contains NUL")
		}
	}
	for key, value := range command.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid command environment %q", key)
		}
	}
	return nil
}

func validateIdentity(kind, value string) error {
	limit := 128
	if kind == "task" {
		limit = 149
	}
	if value == "" || len(value) > limit {
		return fmt.Errorf("sandlock sandbox %s ID must contain 1 to %d characters", kind, limit)
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("sandlock sandbox %s ID %q contains an unsafe character", kind, value)
	}
	return nil
}

func validEnvName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func randomID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type limitedWriter struct {
	writer io.Writer
	wrote  int
	limit  int
}

func (w *limitedWriter) Write(content []byte) (int, error) {
	if len(content) > w.limit-w.wrote {
		return 0, fmt.Errorf("sandlock process output exceeds %d bytes", w.limit)
	}
	written, err := w.writer.Write(content)
	w.wrote += written
	return written, err
}
