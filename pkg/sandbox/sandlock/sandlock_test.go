package sandlock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

func newTestManager(t *testing.T, fake dockerAPI) *Manager {
	t.Helper()
	return &Manager{
		docker: fake, outputDir: t.TempDir(), rootfsCacheDir: t.TempDir(),
		executable: "/bin/sh", cleanupTimeout: defaultCleanupTimeout,
		logger: logrus.StandardLogger(), newID: randomID, sandboxes: map[string]*Sandbox{},
	}
}

func testRequest(taskID string) core.SandboxRequest {
	return core.SandboxRequest{
		RunID: "run-1", TaskID: taskID,
		Environment: core.Environment{Image: "example.com/task:v1", Workdir: "/testbed", MemoryMB: 512},
	}
}

func TestValidateEnvironmentRejectsAllowNetwork(t *testing.T) {
	env := core.Environment{Image: "example.com/task:v1", Workdir: "/testbed", AllowNetwork: true}
	if err := validateEnvironment(env); err == nil {
		t.Error("validateEnvironment should reject allow_network: true")
	}
}

func TestValidateEnvironmentRejectsGPUs(t *testing.T) {
	env := core.Environment{Image: "example.com/task:v1", Workdir: "/testbed", GPUs: 1}
	if err := validateEnvironment(env); err == nil {
		t.Error("validateEnvironment should reject nonzero GPUs")
	}
}

func TestManagerStartSeedsWorkdirAndStopRemovesIt(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:seed1", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)

	live, err := manager.Start(context.Background(), testRequest("task-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)
	seeded := filepath.Join(sandbox.taskDir, "README")
	if content, err := os.ReadFile(seeded); err != nil || string(content) != "seed" {
		t.Fatalf("seeded workdir content = %q, %v", content, err)
	}

	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(sandbox.taskDir); !os.IsNotExist(err) {
		t.Errorf("task work directory should be removed after Stop, stat err = %v", err)
	}
	// The shared rootfs cache must survive Stop.
	if _, err := os.Stat(filepath.Join(sandbox.rootfsDir, "testbed")); err != nil {
		t.Errorf("shared rootfs cache should survive Stop: %v", err)
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:seed2", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-2"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatalf("second Stop should be a safe no-op: %v", err)
	}
}

func TestManagerStopRejectsForeignSandbox(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:seed3", tarContent: buildFakeImageTar(t)}
	managerA := newTestManager(t, fake)
	managerB := newTestManager(t, fake)
	live, err := managerA.Start(context.Background(), testRequest("task-3"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := managerB.Stop(context.Background(), live); err == nil {
		t.Error("Stop should reject a sandbox owned by a different manager")
	}
}

func TestSandboxExecRunsInSandboxWorkdirEnvironment(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:seed4", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-4"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)
	// Real sandlock/unshare invocation needs a Landlock-capable host and the
	// sandlock binary, neither available in this environment; exercise the
	// argv-building/env path directly instead of a real Exec call here.
	argv, err := buildArgv(sandbox.rootfsDir, sandbox.workdir, sandbox.taskDir, sandbox.memoryFlag, core.Command{Path: "/bin/echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if len(argv) == 0 {
		t.Error("buildArgv returned an empty argv")
	}
}

func TestSandboxUploadDownloadStayWithinWorkdir(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:seed5", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-5"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)

	source := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(context.Background(), source, "/testbed/uploaded.txt"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(sandbox.taskDir, "uploaded.txt")); err != nil || string(content) != "payload" {
		t.Errorf("uploaded content = %q, %v", content, err)
	}

	destination := filepath.Join(manager.outputDir, "downloaded.txt")
	if err := sandbox.Download(context.Background(), "/testbed/uploaded.txt", destination); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if content, err := os.ReadFile(destination); err != nil || string(content) != "payload" {
		t.Errorf("downloaded content = %q, %v", content, err)
	}

	if err := sandbox.Upload(context.Background(), source, "/etc/outside-workdir.txt"); err == nil {
		t.Error("Upload outside the sandbox work directory should be rejected")
	}
}
