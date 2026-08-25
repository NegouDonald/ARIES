package sandlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// inContainerLibDir holds the host sandlock binary's shared library
// dependencies (see resolveSandlockRuntime), copied in at a path that never
// collides with a task image's own libc, regardless of what libc (if any)
// that image uses.
const inContainerLibDir = "/.aries-libs"

// resolveSandlockRuntime runs ldd against the host sandlock executable to
// find its ELF interpreter and shared library dependencies, so they can be
// bundled alongside it into task rootfs images that may use a different (or
// no) libc — e.g. Alpine's musl, which lacks the glibc build sandlock's own
// `cargo install` currently produces (as of sandlock-cli v0.8.6, a musl
// target build fails in sandlock's own source, independent of ARIES). A
// statically linked sandlock reports no dependencies here and needs no
// interpreter prefix at exec time, so libraries and interpreterBasename are
// both empty in that case rather than an error.
func resolveSandlockRuntime(ctx context.Context, executable string) (libraries []string, interpreterBasename string, err error) {
	output, err := exec.CommandContext(ctx, "ldd", executable).CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "not a dynamic executable") {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("run ldd on sandlock executable: %w: %s", err, string(output))
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "linux-vdso.so") {
			continue
		}
		var path string
		if idx := strings.Index(line, "=>"); idx >= 0 {
			fields := strings.Fields(strings.TrimSpace(line[idx+2:]))
			if len(fields) == 0 {
				continue
			}
			path = fields[0]
		} else {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			path = fields[0]
		}
		if !strings.HasPrefix(path, "/") {
			continue
		}
		libraries = append(libraries, path)
		if strings.Contains(filepath.Base(path), "ld-linux") {
			interpreterBasename = filepath.Base(path)
		}
	}
	if len(libraries) > 0 && interpreterBasename == "" {
		return nil, "", errors.New("resolve sandlock dynamic dependencies: could not identify the ELF interpreter among ldd's output")
	}
	return libraries, interpreterBasename, nil
}

// embedSandlockRuntime copies the sandlock binary and, if it is dynamically
// linked, its resolved shared libraries into a freshly staged rootfs.
func embedSandlockRuntime(staging, sandlockExecutable string, libraries []string) error {
	if err := copyExecutableFile(sandlockExecutable, filepath.Join(staging, strings.TrimPrefix(inContainerSandlockPath, "/"))); err != nil {
		return fmt.Errorf("embed sandlock binary: %w", err)
	}
	for _, library := range libraries {
		target := filepath.Join(staging, strings.TrimPrefix(inContainerLibDir, "/"), filepath.Base(library))
		if err := copyExecutableFile(library, target); err != nil {
			return fmt.Errorf("embed sandlock dependency %q: %w", library, err)
		}
	}
	return nil
}

// copyExecutableFile copies source to target, explicitly setting the
// executable bit afterward since os.OpenFile's mode argument is still
// filtered by umask on creation.
func copyExecutableFile(source, target string) error {
	content, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %q: %w", source, err)
	}
	defer content.Close()
	if err := writeRegularFile(target, content, 0o755); err != nil {
		return err
	}
	return os.Chmod(target, 0o755)
}
