package sandlock

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// cleanSandboxPath requires an absolute, clean, NUL-free path, mirroring the
// same boundary pkg/sandbox/docker enforces on container paths.
func cleanSandboxPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !strings.HasPrefix(path, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	clean := filepath.Clean(path)
	if clean != path {
		return "", errors.New("path must be clean")
	}
	return clean, nil
}

func requireNonRoot(path string) (string, error) {
	clean, err := cleanSandboxPath(path)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return "", errors.New("path must not be the sandbox root")
	}
	return clean, nil
}

func outputPath(root, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve sandlock download destination: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("sandlock download destination is outside the configured output directory")
	}
	return absolute, nil
}
