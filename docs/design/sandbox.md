# ToolSandbox

`ToolSandbox` owns the task environment. It starts one isolated environment for
a task, returns the narrow live capability needed by the selected bridge and
benchmark, and stops the environment with positive confirmation that owned
resources are absent.

## Boundary and lifecycle

The sandbox begins before bridge or harness startup and stays live through
independent benchmark evaluation. It does not own harness policy, tool
credentials, or benchmark scoring. Cleanup is idempotent, reverse-order, and
bounded after cancellation; failure to confirm resource absence remains a
cleanup failure.

The current implementation uses Docker through the Moby Go SDK. ARIES owns its
containers and networks and never shells out to Docker for lifecycle
operations. A pair-specific bridge may use a narrow sandbox capability such as
streaming command execution, but the harness does not receive Docker daemon
access.

## Customization & Contribution Guide

A new sandbox implementation must preserve exact command argument boundaries,
context-aware external operations, bounded cancellation cleanup, and positive
absence checks. Put it in a concrete package with an explicit constructor and
command switch, then test partial startup, live evaluation, idempotent stop,
resource ownership, and bridge-facing capabilities. Update the supported
reference and operational prerequisites. Do not add registration, discovery,
factories, reflection, DI, or generic plugins.

## Sandlock

`pkg/sandbox/sandlock` implements `ToolSandbox`/`Sandbox` against
[Sandlock](https://github.com/multikernel/sandlock), a Landlock+seccomp
process sandbox, as a non-container alternative to Docker. It is wired into
`cmd/aries` (`sandbox.type: "sandlock"`, see `profiles/hermes-tb2-fix-git-sandlock-deepseek.json`)
and satisfies the same bridge-facing capability surface `pkg/sandbox/docker`
does, so it works with the existing `hermes-ssh`/`openclaw-ssh` bridges
unmodified.

- Sandlock confines literal host filesystem paths, not an image. Each task's
  pinned Docker image is materialized once into a shared, read-only,
  digest-cached directory (Docker `create`+`copy`, the image is never
  started), and only the task's own work directory gets a fresh per-task
  writable copy, bind-mounted over the shared rootfs inside a private,
  unprivileged mount namespace (`unshare --user --map-root-user --mount
  --pid` + `pivot_root`) before Sandlock runs (`pkg/sandbox/sandlock/process.go`).
  This needs unprivileged user namespaces enabled on the host.
- The existing SSH bridges require `NetworkGateway`/`NetworkName`/
  `ContainerID`/`ContainerName`/`ExecStream` on the sandbox, and
  `NetworkGateway` is load-bearing: the bridge listens on it so the harness
  container can reach the sandbox. Since a Sandlock sandbox has no container
  of its own, `pkg/sandbox/sandlock/bridge.go` creates a minimal, `Internal`
  Docker network per task purely for that SSH reachability — the task's own
  process is never attached to it and never runs inside a container;
  `ContainerID`/`ContainerName` are synthetic identifiers for logging only.

Also unlike Docker, `Environment.AllowNetwork` (a boolean) cannot be honored:
Sandlock requires an explicit per-host allow-list, so `Start` rejects
`allow_network: true` outright rather than silently granting broader network
access than requested.

**Not exercised end to end**: this dev environment has no Landlock-active
kernel, no `sandlock` binary, and no Rust toolchain, so the code compiles,
passes `go vet`, and is unit-tested with fakes (`pkg/sandbox/sandlock/*_test.go`),
and a full `aries` profile using it passes every component/pairing check and
image/config validation — but real Landlock/seccomp enforcement, real
`sandlock run` behavior, and `unshare`+`pivot_root` behavior have not been
run on a real Linux 6.12+ host with the `sandlock` CLI installed.
