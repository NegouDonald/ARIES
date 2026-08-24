package sandlock

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type fakeNotFound struct{ kind string }

func (e fakeNotFound) Error() string { return e.kind + " not found" }
func (fakeNotFound) NotFound()       {}

func TestExtractTarRejectsPathEscape(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	mustWriteTarEntry(t, writer, &tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}, "x")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractTar(&buffer, destination); err == nil {
		t.Error("extractTar should reject an entry that escapes the destination")
	}
}

func TestExtractTarWritesFilesDirsAndSymlinks(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	mustWriteTarEntry(t, writer, &tar.Header{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	mustWriteTarEntry(t, writer, &tar.Header{Name: "usr/bin", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	mustWriteTarEntry(t, writer, &tar.Header{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Size: 5, Mode: 0o755}, "hello")
	mustWriteTarEntry(t, writer, &tar.Header{Name: "usr/bin/tool-link", Typeflag: tar.TypeSymlink, Linkname: "tool"}, "")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractTar(&buffer, destination); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "usr/bin/tool"))
	if err != nil || string(content) != "hello" {
		t.Errorf("extracted file content = %q, %v", content, err)
	}
	link, err := os.Readlink(filepath.Join(destination, "usr/bin/tool-link"))
	if err != nil || link != "tool" {
		t.Errorf("extracted symlink = %q, %v", link, err)
	}
}

func mustWriteTarEntry(t *testing.T, writer *tar.Writer, header *tar.Header, content string) {
	t.Helper()
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopyTreeSeedsWorkdir(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "work")
	if err := copyTree(source, destination); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "sub", "file.txt"))
	if err != nil || string(content) != "data" {
		t.Errorf("copied content = %q, %v", content, err)
	}
}

// fakeRootfsClient is a minimal in-memory dockerAPI used to test
// materializeRootfs's caching and the bridge reachability network lifecycle
// without a Docker daemon.
type fakeRootfsClient struct {
	imageID           string
	createCalls       int
	copyCalls         int
	removeCalls       int
	tarContent        []byte
	failContainerCopy bool
	networks          map[string]network.Inspect
}

func (f *fakeRootfsClient) NetworkCreate(_ context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if f.networks == nil {
		f.networks = map[string]network.Inspect{}
	}
	if _, exists := f.networks[name]; exists {
		return client.NetworkCreateResult{}, errors.New("network already exists")
	}
	f.networks[name] = network.Inspect{Network: network.Network{
		Name: name, Labels: options.Labels,
		IPAM: network.IPAM{Config: []network.IPAMConfig{{Gateway: netip.MustParseAddr("172.30.0.1")}}},
	}}
	return client.NetworkCreateResult{ID: name}, nil
}

func (f *fakeRootfsClient) NetworkInspect(_ context.Context, name string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	inspect, ok := f.networks[name]
	if !ok {
		return client.NetworkInspectResult{}, fakeNotFound{"network"}
	}
	return client.NetworkInspectResult{Network: inspect}, nil
}

func (f *fakeRootfsClient) NetworkRemove(_ context.Context, name string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	if _, ok := f.networks[name]; !ok {
		return client.NetworkRemoveResult{}, fakeNotFound{"network"}
	}
	delete(f.networks, name)
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeRootfsClient) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: f.imageID}}, nil
}

func (f *fakeRootfsClient) ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.createCalls++
	return client.ContainerCreateResult{ID: "container-1"}, nil
}

func (f *fakeRootfsClient) CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	f.copyCalls++
	if f.failContainerCopy {
		return client.CopyFromContainerResult{}, errors.New("copy failed")
	}
	return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(f.tarContent))}, nil
}

func (f *fakeRootfsClient) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removeCalls++
	return client.ContainerRemoveResult{}, nil
}

func buildFakeImageTar(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, dir := range []string{"usr", "etc", "testbed"} {
		mustWriteTarEntry(t, writer, &tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	}
	mustWriteTarEntry(t, writer, &tar.Header{Name: "testbed/README", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}, "seed")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestMaterializeRootfsExtractsOnce(t *testing.T) {
	fake := &fakeRootfsClient{imageID: "sha256:abc123", tarContent: buildFakeImageTar(t)}
	cacheDir := t.TempDir()

	destination, err := materializeRootfs(context.Background(), fake, cacheDir, "example.com/task:v1", "/testbed")
	if err != nil {
		t.Fatalf("materializeRootfs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "testbed", "README")); err != nil {
		t.Errorf("extracted rootfs missing seed file: %v", err)
	}
	if fake.copyCalls != 1 {
		t.Errorf("copyCalls = %d, want 1", fake.copyCalls)
	}
	if fake.removeCalls != 1 {
		t.Errorf("removeCalls = %d, want 1 (extraction container must be removed)", fake.removeCalls)
	}

	// A second call for the same image must hit the cache, not extract again.
	second, err := materializeRootfs(context.Background(), fake, cacheDir, "example.com/task:v1", "/testbed")
	if err != nil {
		t.Fatalf("materializeRootfs (cached): %v", err)
	}
	if second != destination {
		t.Errorf("cached materializeRootfs returned a different directory: %q vs %q", second, destination)
	}
	if fake.copyCalls != 1 {
		t.Errorf("copyCalls after cache hit = %d, want still 1", fake.copyCalls)
	}
}

func TestMaterializeRootfsRejectsIncompleteImage(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	mustWriteTarEntry(t, writer, &tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRootfsClient{imageID: "sha256:def456", tarContent: buffer.Bytes()}
	if _, err := materializeRootfs(context.Background(), fake, t.TempDir(), "example.com/task:v1", "/testbed"); err == nil {
		t.Error("materializeRootfs should reject an image missing /usr")
	}
}
