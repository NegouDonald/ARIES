package sandlock

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// rootfsClient is the small Engine surface used only to materialize a pinned
// image's filesystem onto disk. The extraction container is created and
// inspected but never started, so a task's code never runs inside Docker.
type rootfsClient interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

const rootfsCompleteMarker = ".aries-rootfs-complete"

// materializeRootfs extracts image's filesystem to a digest-keyed directory
// under cacheDir, reusing a prior extraction of the same image. The returned
// directory is shared and read-only from the caller's perspective: it may be
// read by concurrent tasks using the same image, but must never be written to
// after this call returns, since doing so would corrupt other tasks' view of
// it. It also ensures workdir exists inside the extracted tree, since that
// path is later used as a bind-mount target for each task's own writable copy,
// and it copies the host sandlock binary in at inContainerSandlockPath: a
// bind-mount of the binary performed just before pivot_root proved fragile in
// practice (the mount didn't reliably survive the pivot with usable exec
// permission), whereas a plain file baked into the cached tree at
// materialization time is not exposed to that timing at all.
func materializeRootfs(ctx context.Context, api rootfsClient, cacheDir, image, workdir, sandlockExecutable string) (string, error) {
	inspection, err := api.ImageInspect(ctx, image)
	if err != nil {
		return "", fmt.Errorf("inspect sandlock rootfs image %q: %w", image, err)
	}
	if inspection.ID == "" {
		return "", fmt.Errorf("inspect sandlock rootfs image %q: empty image identity", image)
	}
	key := strings.NewReplacer("/", "_", ":", "_").Replace(inspection.ID)
	destination := filepath.Join(cacheDir, key)
	if _, err := os.Stat(filepath.Join(destination, rootfsCompleteMarker)); err == nil {
		return destination, nil
	}

	staging, err := os.MkdirTemp(cacheDir, ".aries-rootfs-*")
	if err != nil {
		return "", fmt.Errorf("create sandlock rootfs staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: image}})
	if err != nil {
		return "", fmt.Errorf("create sandlock rootfs extraction container: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("create sandlock rootfs extraction container: Docker returned an empty container ID")
	}
	defer api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})

	result, err := api.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return "", fmt.Errorf("copy sandlock rootfs from extraction container: %w", err)
	}
	defer result.Content.Close()
	if err := extractTar(result.Content, staging); err != nil {
		return "", fmt.Errorf("extract sandlock rootfs: %w", err)
	}
	for _, required := range []string{"usr", "etc"} {
		if info, err := os.Stat(filepath.Join(staging, required)); err != nil || !info.IsDir() {
			return "", fmt.Errorf("extract sandlock rootfs: extracted tree is missing /%s", required)
		}
	}
	if err := os.MkdirAll(filepath.Join(staging, strings.TrimPrefix(workdir, "/")), 0o755); err != nil {
		return "", fmt.Errorf("prepare sandlock rootfs workdir: %w", err)
	}
	if err := copySandlockBinary(sandlockExecutable, filepath.Join(staging, strings.TrimPrefix(inContainerSandlockPath, "/"))); err != nil {
		return "", fmt.Errorf("embed sandlock binary in rootfs: %w", err)
	}

	if err := os.RemoveAll(destination); err != nil {
		return "", fmt.Errorf("clear stale sandlock rootfs cache entry: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		return "", fmt.Errorf("publish sandlock rootfs cache entry: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destination, rootfsCompleteMarker), nil, 0o600); err != nil {
		return "", fmt.Errorf("mark sandlock rootfs cache entry complete: %w", err)
	}
	return destination, nil
}

// extractTar writes a tar stream's directories, regular files, symlinks, and
// hard links beneath destination, rejecting any entry that would resolve
// outside it. Device nodes, FIFOs, and other special files are skipped: they
// cannot be created without host privilege, and Sandlock's own policy denies
// access to anything not explicitly granted regardless.
func extractTar(source io.Reader, destination string) error {
	reader := tar.NewReader(source)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		target, err := resolveTarget(destination, name)
		if err != nil {
			return fmt.Errorf("tar entry %q: %w", header.Name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeRegularFile(target, reader, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkTarget, err := resolveTarget(destination, strings.TrimPrefix(strings.TrimPrefix(header.Linkname, "./"), "/"))
			if err != nil {
				return fmt.Errorf("tar entry %q link target: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
		default:
			continue
		}
	}
}

func resolveTarget(destination, name string) (string, error) {
	target := filepath.Join(destination, name)
	relative, err := filepath.Rel(destination, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("escapes destination")
	}
	return target, nil
}

func writeRegularFile(target string, content io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// copySandlockBinary copies the host sandlock executable into the rootfs
// being materialized, explicitly setting the executable bit afterward since
// os.OpenFile's mode argument is still filtered by umask on creation.
func copySandlockBinary(sandlockExecutable, target string) error {
	source, err := os.Open(sandlockExecutable)
	if err != nil {
		return fmt.Errorf("open host sandlock executable: %w", err)
	}
	defer source.Close()
	if err := writeRegularFile(target, source, 0o755); err != nil {
		return err
	}
	return os.Chmod(target, 0o755)
}

// seedWorkdir copies workdir's content from the shared, read-only rootfs into
// a fresh per-task directory. It is the only per-task write a task's own
// files require: the copy is bind-mounted over workdir inside each sandboxed
// process's private mount namespace, so concurrent tasks sharing the same
// cached rootfs never see each other's writes.
func seedWorkdir(rootfsDir, workdir, taskDir string) error {
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return fmt.Errorf("create sandlock task work directory: %w", err)
	}
	source := filepath.Join(rootfsDir, strings.TrimPrefix(workdir, "/"))
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat sandlock rootfs workdir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandlock sandbox workdir %q is not a directory in the task image", workdir)
	}
	return copyTree(source, taskDir)
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			defer source.Close()
			return writeRegularFile(target, source, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func validatePinnedImage(image string) error {
	if err := containerimage.Validate(image); err == nil {
		return nil
	}
	tagged, err := containerimage.ValidateTagOnly(image)
	if err != nil {
		return err
	}
	if tagged != image {
		return errors.New("image must not contain surrounding whitespace")
	}
	return nil
}
