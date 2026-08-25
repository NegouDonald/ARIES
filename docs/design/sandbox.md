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
  This needs unprivileged user namespaces enabled on the host; on Ubuntu
  24.04+ that also means `kernel.apparmor_restrict_unprivileged_userns` must
  allow it (AppArmor gates unprivileged user namespace creation independently
  of the older `kernel.unprivileged_userns_clone` sysctl).
- sandlock's own `cargo install` build is dynamically linked against glibc,
  and sandlock's source does not currently build for the musl target (an
  upstream `libc`-crate type mismatch between glibc's and musl's `ioctl`/
  `ptrace` bindings, as of sandlock-cli v0.8.6 — unrelated to ARIES). A task
  image with a different or absent libc (Alpine's musl, for one) can't run
  the embedded sandlock binary directly, so `pkg/sandbox/sandlock/runtime.go`
  resolves sandlock's shared library dependencies once via `ldd` and embeds
  them at a fixed, collision-free in-rootfs path, invoking sandlock through
  its own bundled ELF interpreter with an explicit `--library-path`.
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
access than requested. In practice this means Sandlock currently cannot run
any Terminal-Bench 2 task from the pinned `terminal-bench-2` revision, since
every task in that checkout sets `allow_internet = true`; running a real task
end to end needs either a task suite with network-free tasks or a future
per-host allow-list mechanism on top of `Environment`.

**Verified end to end** on a real Linux 6.12+ host (kernel 7.0.0, Landlock
active, `apparmor_restrict_unprivileged_userns` disabled) with the `sandlock`
CLI installed: `Manager.Start` against a plain `alpine:3.19` image followed
by `Sandbox.Exec` of `uname -a; id; cat /etc/os-release` returned exit code 0
with output confirming the command ran genuinely confined inside the
materialized Alpine rootfs, followed by a clean `Manager.Stop`. This was
exercised directly against the package (bypassing Terminal-Bench 2, per the
`allow_internet` limitation above), not yet through a full `aries` profile
run with a real benchmark task.
