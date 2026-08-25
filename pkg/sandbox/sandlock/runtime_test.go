package sandlock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSandlockRuntimeAgainstADynamicBinary(t *testing.T) {
	// /bin/sh is dynamically linked on every Linux distribution this package
	// targets, so this exercises the real ldd parsing path without needing
	// the actual sandlock binary in this environment.
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	libraries, interpreter, err := resolveSandlockRuntime(context.Background(), shell)
	if err != nil {
		t.Fatalf("resolveSandlockRuntime: %v", err)
	}
	if len(libraries) == 0 {
		t.Fatal("expected at least one resolved library for a dynamically linked binary")
	}
	if interpreter == "" {
		t.Error("expected a non-empty interpreter basename for a dynamically linked binary")
	}
	for _, library := range libraries {
		if _, err := os.Stat(library); err != nil {
			t.Errorf("resolved library %q does not exist on disk: %v", library, err)
		}
	}
}

func TestResolveSandlockRuntimeRejectsMissingExecutable(t *testing.T) {
	if _, _, err := resolveSandlockRuntime(context.Background(), "/no/such/executable"); err == nil {
		t.Error("expected an error for a nonexistent executable")
	}
}

func TestEmbedSandlockRuntime(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	libraries, _, err := resolveSandlockRuntime(context.Background(), shell)
	if err != nil {
		t.Fatalf("resolveSandlockRuntime: %v", err)
	}
	staging := t.TempDir()
	if err := embedSandlockRuntime(staging, shell, libraries); err != nil {
		t.Fatalf("embedSandlockRuntime: %v", err)
	}
	binary, err := os.Stat(filepath.Join(staging, strings.TrimPrefix(inContainerSandlockPath, "/")))
	if err != nil {
		t.Fatalf("embedded binary missing: %v", err)
	}
	if binary.Mode().Perm()&0o111 == 0 {
		t.Errorf("embedded binary is not executable: mode %v", binary.Mode())
	}
	for _, library := range libraries {
		target := filepath.Join(staging, strings.TrimPrefix(inContainerLibDir, "/"), filepath.Base(library))
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("embedded library %q missing: %v", target, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("embedded library %q is not executable: mode %v", target, info.Mode())
		}
	}
}
