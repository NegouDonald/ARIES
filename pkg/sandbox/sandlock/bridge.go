package sandlock

import (
	"context"
	"errors"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
)

// networkClient is the small Engine surface used only to give the existing
// Docker-network-shaped SSH bridges (pkg/bridge/openclawssh, pkg/bridge/hermesssh)
// something to attach the harness container to and a gateway IP to reach.
// It has nothing to do with the sandboxed task's own network access: the task
// process is never attached to this network, and its network access is
// governed entirely by Sandlock's own policy on the host network namespace it
// actually runs in (see process.go). This network exists purely as SSH
// reachability plumbing between the harness container and this host process.
type networkClient interface {
	NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

// dockerAPI is the full Engine surface this package uses: rootfsClient for
// one-time image materialization, networkClient for bridge reachability.
type dockerAPI interface {
	rootfsClient
	networkClient
}

func bridgeNetworkLabels(request core.SandboxRequest) map[string]string {
	return map[string]string{
		"aries.managed": "true", "aries.kind": "sandlock-bridge-network",
		"aries.run": request.RunID, "aries.task": request.TaskID,
	}
}

// createReachabilityNetwork creates a Docker network solely so a Docker-network
// SSH bridge can reach this sandbox. It is marked Internal because it need
// never route to the internet: it only carries the harness-to-bridge SSH
// tunnel, never the sandboxed task's own traffic.
func createReachabilityNetwork(ctx context.Context, api networkClient, request core.SandboxRequest, networkName string) error {
	if _, err := api.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Internal: true, Labels: bridgeNetworkLabels(request),
	}); err != nil {
		return fmt.Errorf("create sandlock bridge reachability network: %w", err)
	}
	return nil
}

func sameNetworkIdentity(labels map[string]string, runID, taskID string) bool {
	return labels["aries.managed"] == "true" && labels["aries.run"] == runID && labels["aries.task"] == taskID
}

// removeReachabilityNetwork removes the sandbox's bridge reachability network
// and positively confirms it is gone, matching pkg/sandbox/docker's cleanup
// discipline.
func removeReachabilityNetwork(ctx context.Context, api networkClient, networkName string) error {
	_, err := api.NetworkRemove(ctx, networkName, client.NetworkRemoveOptions{})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove sandlock bridge reachability network: %w", err)
	}
	_, err = api.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err == nil {
		return errors.New("confirm sandlock bridge reachability network absence: network still exists")
	}
	return fmt.Errorf("confirm sandlock bridge reachability network absence: %w", err)
}

// NetworkGateway returns the IPv4 gateway of the sandbox's bridge
// reachability network, matching pkg/sandbox/docker's Sandbox.NetworkGateway
// so the existing SSH bridges can bind their listener to it unmodified.
func (s *Sandbox) NetworkGateway(ctx context.Context) (string, error) {
	result, err := s.owner.docker.NetworkInspect(ctx, s.networkName, client.NetworkInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect sandlock bridge reachability network gateway: %w", err)
	}
	if !sameNetworkIdentity(result.Network.Labels, s.runID, s.taskID) {
		return "", errors.New("inspect sandlock bridge reachability network gateway: identity labels do not match")
	}
	for _, config := range result.Network.IPAM.Config {
		if config.Gateway.Is4() {
			return config.Gateway.String(), nil
		}
	}
	return "", errors.New("inspect sandlock bridge reachability network gateway: no IPv4 gateway")
}

// NetworkName returns the task-scoped Docker network used for bridge
// reachability only.
func (s *Sandbox) NetworkName() string { return s.networkName }

// ContainerID and ContainerName are synthetic: a Sandlock sandbox runs the
// task as a host process, not a Docker container, so there is no real Docker
// container identity to report. They exist only so this sandbox satisfies the
// same logging/audit surface pkg/sandbox/docker's Sandbox does for the
// existing SSH bridges; nothing downstream should treat them as real Docker
// container identifiers.
func (s *Sandbox) ContainerID() string { return "sandlock:" + s.id }

func (s *Sandbox) ContainerName() string { return "aries-sandlock-" + s.id }

// RunID returns the owning experiment run identity for bridge tool logs.
func (s *Sandbox) RunID() string { return s.runID }

// TaskID returns the owning benchmark task identity for bridge tool logs.
func (s *Sandbox) TaskID() string { return s.taskID }

// Workdir returns the benchmark-declared task working directory.
func (s *Sandbox) Workdir() string { return s.workdir }

// ExecStream is the bridge-facing streaming form of Exec. Unlike Docker,
// which must work around a remote daemon's exec API to get live stdin/stdout
// piping, this runs the process directly, so streaming falls out of runProcess
// wiring cmd.Stdin/Stdout/Stderr to whatever readers/writers the bridge
// passes in.
func (s *Sandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1}, fmt.Errorf("invalid sandlock command: %w", err)
	}
	argv, err := buildArgv(s.rootfsDir, s.workdir, s.taskDir, s.memoryFlag, command)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	execCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	onStart := func(pgid int) {
		s.mu.Lock()
		s.activePGID = pgid
		s.mu.Unlock()
	}
	result, err := runProcess(execCtx, argv, buildEnv(command.Env), stdin,
		&limitedWriter{writer: stdout, limit: maxExecOutput},
		&limitedWriter{writer: stderr, limit: maxExecOutput},
		onStart)
	s.mu.Lock()
	s.activePGID = 0
	s.mu.Unlock()
	return result, err
}
