package sandlock

import (
	"context"
	"io"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Compile-time contract check: *Sandbox must expose every method the
// existing SSH bridges (pkg/bridge/openclawssh, pkg/bridge/hermesssh)
// type-assert for on their unexported bridgeSandbox interface. That
// interface is redeclared here, not imported, purely to verify the shape.
var _ interface {
	Exec(context.Context, core.Command) (core.CommandResult, error)
	Upload(context.Context, string, string) error
	Download(context.Context, string, string) error
	ContainerID() string
	ContainerName() string
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
} = (*Sandbox)(nil)

func TestSandboxNetworkGateway(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:bridge1", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-bridge-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)

	gateway, err := sandbox.NetworkGateway(context.Background())
	if err != nil {
		t.Fatalf("NetworkGateway: %v", err)
	}
	if gateway != "172.30.0.1" {
		t.Errorf("gateway = %q, want 172.30.0.1", gateway)
	}
	if sandbox.NetworkName() == "" {
		t.Error("NetworkName is empty")
	}
	if sandbox.ContainerID() == "" || sandbox.ContainerName() == "" {
		t.Error("ContainerID/ContainerName should be nonempty synthetic identifiers")
	}
	if sandbox.RunID() != "run-1" || sandbox.TaskID() != "task-bridge-1" || sandbox.Workdir() != "/testbed" {
		t.Errorf("identity getters = %q %q %q", sandbox.RunID(), sandbox.TaskID(), sandbox.Workdir())
	}
}

func TestManagerStopRemovesReachabilityNetwork(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:bridge2", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-bridge-2"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)
	if _, ok := fake.networks[sandbox.networkName]; !ok {
		t.Fatal("reachability network was not created")
	}

	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := fake.networks[sandbox.networkName]; ok {
		t.Error("reachability network should be removed after Stop")
	}
}

func TestNetworkGatewayRejectsMismatchedIdentity(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:bridge3", tarContent: buildFakeImageTar(t)}
	manager := newTestManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest("task-bridge-3"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sandbox := live.(*Sandbox)
	// Tamper with the fake's stored labels to simulate a network that
	// resolves to the right name but the wrong ownership.
	entry := fake.networks[sandbox.networkName]
	entry.Labels = map[string]string{"aries.managed": "true", "aries.run": "someone-else", "aries.task": "task-bridge-3"}
	fake.networks[sandbox.networkName] = entry

	if _, err := sandbox.NetworkGateway(context.Background()); err == nil {
		t.Error("NetworkGateway should reject a network with mismatched ownership labels")
	}
}
