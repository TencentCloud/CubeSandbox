// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestNativeRootfsExportEnabledParsesEnv(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "")
	if !nativeRootfsExportEnabled() {
		t.Fatal("expected native rootfs export to be enabled by default")
	}

	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "false")
	if nativeRootfsExportEnabled() {
		t.Fatal("expected native rootfs export to be disabled")
	}

	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "not-a-bool")
	if !nativeRootfsExportEnabled() {
		t.Fatal("expected invalid native export env value to fallback to enabled")
	}
}

func TestNativeExportConcurrencyParsesEnv(t *testing.T) {
	t.Setenv(nativeExportJobsEnv, "")
	if got := nativeExportConcurrency(); got != defaultNativeExportConcurrency {
		t.Fatalf("default concurrency=%d, want %d", got, defaultNativeExportConcurrency)
	}

	t.Setenv(nativeExportJobsEnv, "12")
	if got := nativeExportConcurrency(); got != 12 {
		t.Fatalf("configured concurrency=%d, want 12", got)
	}

	t.Setenv(nativeExportJobsEnv, "0")
	if got := nativeExportConcurrency(); got != defaultNativeExportConcurrency {
		t.Fatalf("invalid concurrency=%d, want %d", got, defaultNativeExportConcurrency)
	}

	t.Setenv(nativeExportJobsEnv, "128")
	if got := nativeExportConcurrency(); got != maxNativeExportConcurrency {
		t.Fatalf("capped concurrency=%d, want %d", got, maxNativeExportConcurrency)
	}
}

func TestPrepareNativeSourceExtractsDigestAndConfig(t *testing.T) {
	s := httptest.NewServer(registry.New())
	defer s.Close()

	// Create a dummy image
	img, err := mutate.Config(empty.Image, v1.Config{
		Cmd: []string{"/bin/sh"},
	})
	if err != nil {
		t.Fatalf("mutate.Config: %v", err)
	}

	// Create a dummy layer
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "test.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("test"))
	tw.Close()
	layer, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b.Bytes())), nil
	})
	img, _ = mutate.AppendLayers(img, layer)

	ref, _ := name.ParseReference(s.URL[7:] + "/test-native:latest")
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	spec := SourceSpec{
		ImageRef:         "docker://" + ref.Name(),
		RegistryUsername: "",
		RegistryPassword: "",
	}

	source, err := prepareNativeSource(context.Background(), spec)
	if err != nil {
		t.Fatalf("prepareNativeSource failed: %v", err)
	}

	if source.ExportMode != ExportModeNative {
		t.Errorf("expected ExportMode=ExportModeNative, got %q", source.ExportMode)
	}
	if len(source.Config.Cmd) == 0 || source.Config.Cmd[0] != "/bin/sh" {
		t.Errorf("expected Config.Cmd=[/bin/sh], got %v", source.Config.Cmd)
	}

	digest, _ := img.Digest()
	expectedDigest := s.URL[7:] + "/test-native@" + digest.String()
	if source.Digest != expectedDigest {
		t.Errorf("expected Digest=%q, got %q", expectedDigest, source.Digest)
	}

	// We appended 1 layer, let's just make sure CompressedSizeBytes > 0
	if source.CompressedSizeBytes <= 0 {
		t.Errorf("expected CompressedSizeBytes > 0, got %d", source.CompressedSizeBytes)
	}
	if source.RegistryAuth != nil {
		t.Errorf("expected RegistryAuth to be nil without explicit credentials, got %#v", source.RegistryAuth)
	}
}

func TestPrepareSourceUsesNativeWhenEnabled(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")

	img, err := mutate.Config(empty.Image, v1.Config{
		Cmd: []string{"native"},
	})
	if err != nil {
		t.Fatalf("mutate.Config: %v", err)
	}

	tests := []struct {
		name string
		fn   func(context.Context, SourceSpec) (*PreparedSource, error)
	}{
		{name: "PrepareSource", fn: PrepareSource},
		{name: "PrepareLocalSource", fn: PrepareLocalSource},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patches := gomonkey.ApplyFuncReturn(remote.Image, img, nil)
			defer patches.Reset()
			source, err := tt.fn(context.Background(), SourceSpec{
				ImageRef:         "example.com/native:latest",
				RegistryUsername: "user",
				RegistryPassword: "pass",
			})
			if err != nil {
				t.Fatalf("%s failed: %v", tt.name, err)
			}
			if source.ExportMode != ExportModeNative {
				t.Fatalf("ExportMode=%q, want %q", source.ExportMode, ExportModeNative)
			}
			if source.RegistryAuth == nil || source.RegistryAuth.Username != "user" || source.RegistryAuth.Password != "pass" {
				t.Fatalf("unexpected RegistryAuth: %#v", source.RegistryAuth)
			}
			if len(source.Config.Cmd) != 1 || source.Config.Cmd[0] != "native" {
				t.Fatalf("Config.Cmd=%v, want [native]", source.Config.Cmd)
			}
			if source.nativeImage == nil {
				t.Fatal("expected prepared native image to be cached")
			}
		})
	}
}

func TestImageDigestFromReferenceMatchesDockerlessCanonicalName(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		want     string
	}{
		{
			name:     "docker hub short name",
			imageRef: "nginx:latest",
			want:     "docker.io/library/nginx@sha256:abcd",
		},
		{
			name:     "docker hub explicit alias",
			imageRef: "docker.io/library/nginx:latest",
			want:     "docker.io/library/nginx@sha256:abcd",
		},
		{
			name:     "non docker hub registry",
			imageRef: "example.com/ns/app:stable",
			want:     "example.com/ns/app@sha256:abcd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := name.ParseReference(tt.imageRef)
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tt.imageRef, err)
			}
			if got := imageDigestFromReference(ref, "sha256:abcd"); got != tt.want {
				t.Fatalf("imageDigestFromReference(%q)=%q, want %q", tt.imageRef, got, tt.want)
			}
		})
	}
}

func TestStreamRegistryWhiteoutResolution(t *testing.T) {
	// Base layer: creates /dir/file1 and /dir/file2
	var b1 bytes.Buffer
	tw1 := tar.NewWriter(&b1)
	tw1.WriteHeader(&tar.Header{Name: "dir/", Mode: 0755, Typeflag: tar.TypeDir})
	tw1.WriteHeader(&tar.Header{Name: "dir/file1", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.WriteHeader(&tar.Header{Name: "dir/file2", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.Close()
	l1, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b1.Bytes())), nil
	})

	// Second layer: deletes /dir/file1 via whiteout, and makes an opaque dir marker /dir/.wh..wh..opq
	var b2 bytes.Buffer
	tw2 := tar.NewWriter(&b2)
	tw2.WriteHeader(&tar.Header{Name: "dir/.wh.file1", Size: 0, Mode: 0644})
	tw2.WriteHeader(&tar.Header{Name: "dir/.wh..wh..opq", Size: 0, Mode: 0644})
	tw2.WriteHeader(&tar.Header{Name: "dir/file3", Size: 4, Mode: 0644}) // New file
	tw2.Write([]byte("data"))
	tw2.Close()
	l2, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b2.Bytes())), nil
	})

	img, _ := mutate.AppendLayers(empty.Image, l1, l2)

	destDir := t.TempDir()
	source := &PreparedSource{
		LocalRef:    "docker.io/library/test-whiteout:latest",
		nativeImage: img,
	}

	err := StreamRegistryToDir(context.Background(), source, destDir)
	if err != nil {
		t.Fatalf("StreamRegistryToDir failed: %v", err)
	}

	// Verify the result
	// file1 should be deleted by whiteout
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file1")); !os.IsNotExist(err) {
		t.Errorf("expected dir/file1 to be deleted by whiteout, but it exists")
	}
	// file2 should be deleted by the opaque directory marker
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file2")); !os.IsNotExist(err) {
		t.Errorf("expected dir/file2 to be deleted by opaque dir marker, but it exists")
	}
	// file3 should exist
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file3")); err != nil {
		t.Errorf("expected dir/file3 to exist, but got: %v", err)
	}
}

func TestStreamRegistryUsesPreparedNativeImage(t *testing.T) {
	var b1 bytes.Buffer
	tw1 := tar.NewWriter(&b1)
	tw1.WriteHeader(&tar.Header{Name: "dir/", Mode: 0755, Typeflag: tar.TypeDir})
	tw1.WriteHeader(&tar.Header{Name: "dir/file1", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.Close()
	l1, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b1.Bytes())), nil
	})

	img, _ := mutate.AppendLayers(empty.Image, l1)

	patches := gomonkey.ApplyFunc(remote.Image, func(name.Reference, ...remote.Option) (v1.Image, error) {
		t.Fatal("remote.Image should not be called when nativeImage is already cached")
		return nil, nil
	})
	t.Cleanup(func() {
		patches.Reset()
	})

	source := &PreparedSource{
		LocalRef:    "docker.io/library/test-native:latest",
		nativeImage: img,
	}

	destDir := t.TempDir()
	if err := StreamRegistryToDir(context.Background(), source, destDir); err != nil {
		t.Fatalf("StreamRegistryToDir failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "dir", "file1")); err != nil {
		t.Fatalf("expected dir/file1 to exist, but got: %v", err)
	}
}

func TestStreamRegistryCancelWhileSchedulingReturnsContextError(t *testing.T) {
	t.Setenv(nativeExportJobsEnv, "1")

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		canceled := make(chan struct{})
		release := make(chan struct{})
		firstLayer := testCompressedLayer{
			rc: &cancelOnReadCloser{
				cancel:   cancel,
				canceled: canceled,
				release:  release,
			},
		}
		secondLayer := testCompressedLayer{
			rc: io.NopCloser(bytes.NewReader(nil)),
		}

		destDir := t.TempDir()
		done := make(chan error, 1)
		go func() {
			done <- StreamRegistryToDir(ctx, &PreparedSource{
				LocalRef: "docker.io/library/test-native:latest",
				nativeImage: testImage{
					Image:  empty.Image,
					layers: []v1.Layer{firstLayer, secondLayer},
				},
			}, destDir)
		}()

		<-canceled
		close(release)

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamRegistryToDir error=%v, want context.Canceled", err)
		}
	})
}

type testImage struct {
	v1.Image
	layers []v1.Layer
}

func (i testImage) Layers() ([]v1.Layer, error) {
	return i.layers, nil
}

type testCompressedLayer struct {
	v1.Layer
	rc io.ReadCloser
}

func (l testCompressedLayer) Compressed() (io.ReadCloser, error) {
	return l.rc, nil
}

type cancelOnReadCloser struct {
	cancel   context.CancelFunc
	canceled chan struct{}
	release  chan struct{}
}

func (r *cancelOnReadCloser) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
		close(r.canceled)
		r.cancel = nil
	}
	<-r.release
	return 0, io.EOF
}

func (r *cancelOnReadCloser) Close() error {
	return nil
}

func TestStreamRegistryConcurrencyAndPipelining(t *testing.T) {
	makeTar := func(filename string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: filename, Size: 4, Mode: 0644})
		_, _ = tw.Write([]byte("data"))
		_ = tw.Close()
		return buf.Bytes()
	}

	// Verify that the semaphore correctly limits concurrent downloads.
	t.Run("ConcurrencyLimit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(nativeExportJobsEnv, "2")

			gates := [3]chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			started := [3]chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			layers := []v1.Layer{
				gatedCompressedLayer{data: makeTar("a.txt"), gate: gates[0], started: started[0]},
				gatedCompressedLayer{data: makeTar("b.txt"), gate: gates[1], started: started[1]},
				gatedCompressedLayer{data: makeTar("c.txt"), gate: gates[2], started: started[2]},
			}

			destDir := t.TempDir()
			var streamErr error
			go func() {
				streamErr = StreamRegistryToDir(context.Background(), &PreparedSource{
					LocalRef: "test:latest", nativeImage: testImage{Image: empty.Image, layers: layers},
				}, destDir)
			}()
			synctest.Wait()

			// With jobs=2, exactly 2 goroutines should have acquired the semaphore.
			count := 0
			for _, ch := range started {
				select {
				case <-ch:
					count++
				default:
				}
			}
			if count != 2 {
				t.Fatalf("concurrent downloads = %d, want 2", count)
			}

			for i := range gates {
				close(gates[i])
			}
			synctest.Wait()

			if streamErr != nil {
				t.Fatalf("StreamRegistryToDir: %v", streamErr)
			}
		})
	})

	// Verify that layers are extracted in order even when downloads complete out of order.
	t.Run("OrderPreservingExtraction", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Set jobs >= numLayers so all goroutines acquire the semaphore immediately,
			// eliminating scheduling non-determinism and letting us focus on extraction ordering.
			t.Setenv(nativeExportJobsEnv, "3")

			gates := [3]chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			layers := []v1.Layer{
				gatedCompressedLayer{data: makeTar("layer0.txt"), gate: gates[0]},
				gatedCompressedLayer{data: makeTar("layer1.txt"), gate: gates[1]},
				gatedCompressedLayer{data: makeTar("layer2.txt"), gate: gates[2]},
			}

			destDir := t.TempDir()
			var streamErr error
			go func() {
				streamErr = StreamRegistryToDir(context.Background(), &PreparedSource{
					LocalRef: "test:latest", nativeImage: testImage{Image: empty.Image, layers: layers},
				}, destDir)
			}()
			synctest.Wait()

			// Release layer 2 first (out of order).
			// Phase 2 is blocked waiting for layer 0, so nothing should be extracted.
			close(gates[2])
			synctest.Wait()
			assertNotExists(t, filepath.Join(destDir, "layer2.txt"))

			// Release layer 0.
			// Phase 2 extracts layer 0, then blocks waiting for layer 1.
			close(gates[0])
			synctest.Wait()
			assertExists(t, filepath.Join(destDir, "layer0.txt"))
			assertNotExists(t, filepath.Join(destDir, "layer1.txt"))
			assertNotExists(t, filepath.Join(destDir, "layer2.txt"))

			// Release layer 1.
			// Phase 2 extracts layer 1, then immediately extracts layer 2 (already downloaded).
			close(gates[1])
			synctest.Wait()
			assertExists(t, filepath.Join(destDir, "layer1.txt"))
			assertExists(t, filepath.Join(destDir, "layer2.txt"))

			if streamErr != nil {
				t.Fatalf("StreamRegistryToDir: %v", streamErr)
			}
		})
	})
}

// --- Test helpers for gated layers ---

// gatedCompressedLayer is a v1.Layer whose Compressed() returns a reader that
// blocks on a gate channel, giving the test precise control over download completion order.
type gatedCompressedLayer struct {
	v1.Layer
	data    []byte
	gate    chan struct{} // close to unblock the download
	started chan struct{} // optional; closed on first Read to signal sem acquisition
}

func (l gatedCompressedLayer) Compressed() (io.ReadCloser, error) {
	return &gatedReader{data: l.data, gate: l.gate, started: l.started}, nil
}

type gatedReader struct {
	data       []byte
	gate       chan struct{}
	started    chan struct{}
	gateOpened bool
	reader     *bytes.Reader
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if !r.gateOpened {
		if r.started != nil {
			close(r.started)
		}
		<-r.gate
		r.gateOpened = true
		r.reader = bytes.NewReader(r.data)
	}
	return r.reader.Read(p)
}

func (r *gatedReader) Close() error { return nil }

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", filepath.Base(path), err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to NOT exist", filepath.Base(path))
	}
}
