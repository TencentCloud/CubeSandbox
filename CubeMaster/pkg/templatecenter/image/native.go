// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
)

const (
	defaultNativeExportConcurrency = 6
	maxNativeExportConcurrency     = 32
	nativeExportJobsEnv            = "CUBEMASTER_NATIVE_ROOTFS_EXPORT_JOBS"
	// nativeCopyBufferSize is the buffer size for I/O operations. 1MB is chosen as a
	// good balance between memory usage and read/write system call overhead.
	nativeCopyBufferSize = 1024 * 1024
)

var nativeCopyBufferPool = sync.Pool{
	New: func() any {
		return make([]byte, nativeCopyBufferSize)
	},
}

var (
	googleDefaultTokenSource                = google.DefaultTokenSource
	defaultKeychain          authn.Keychain = authn.DefaultKeychain // seam for tests
)

// nativeRootfsExportEnabled checks if CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED is enabled.
// By default, it is enabled, which avoids using external CLI tools (docker, skopeo, umoci).
func nativeRootfsExportEnabled() bool {
	v, err := strconv.ParseBool(os.Getenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED"))
	return err != nil || v
}

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// registryAuthenticator resolves registry auth for a pull. Explicit credentials
// always win. For Google Artifact Registry, the docker keychain wins when it
// has credentials; otherwise Application Default Credentials (including GKE
// Workload Identity) are used. Other registries retain the DefaultKeychain
// fallback.
func registryAuthenticator(ctx context.Context, ref name.Reference, auth *RegistryAuthConfig) (authn.Authenticator, error) {
	if auth != nil && (auth.Username != "" || auth.Password != "") {
		return authn.FromConfig(authn.AuthConfig{
			Username: auth.Username,
			Password: auth.Password,
		}), nil
	}
	registry := ref.Context()
	// Docker-config credentials win over ADC; keychain errors propagate.
	resolved, err := defaultKeychain.Resolve(registry)
	if err != nil {
		return nil, err
	}
	if resolved != authn.Anonymous {
		return resolved, nil
	}
	if isGoogleArtifactRegistry(registry.RegistryStr()) {
		source, err := googleDefaultTokenSource(ctx, cloudPlatformScope)
		if err == nil {
			return &tokenSourceAuthenticator{source: source}, nil
		}
		// ADC is unavailable (e.g. not on GCP, Workload Identity misconfigured).
		// Fall through to anonymous, but log so this is distinguishable from a
		// permission problem at the registry.
		log.G(ctx).Debugf("Artifact Registry ADC unavailable for %s: %v", registry.RegistryStr(), err)
	}
	return authn.Anonymous, nil
}

// tokenSourceAuthenticator presents an OAuth2 access token as the
// `oauth2accesstoken` username that Artifact Registry expects. Token() is
// invoked on every Authorization() so the underlying ReuseTokenSource can
// refresh the token if a pull outlives its ~1h lifetime.
type tokenSourceAuthenticator struct {
	source oauth2.TokenSource
}

func (a *tokenSourceAuthenticator) Authorization() (*authn.AuthConfig, error) {
	token, err := a.source.Token()
	if err != nil {
		return nil, err
	}
	if token.AccessToken == "" {
		return nil, errors.New("ADC returned empty access token for Artifact Registry")
	}
	return &authn.AuthConfig{
		Username: "oauth2accesstoken",
		Password: token.AccessToken,
	}, nil
}

func isGoogleArtifactRegistry(registry string) bool {
	return strings.HasSuffix(strings.ToLower(registry), ".pkg.dev")
}

type progressReader struct {
	io.Reader
	onRead func(n int)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.Reader.Read(p)
	if n > 0 && pr.onRead != nil {
		pr.onRead(n)
	}
	return n, err
}

// StreamRegistryToDir fetches and applies OCI layers directly into destDir.
func StreamRegistryToDir(ctx context.Context, source *PreparedSource, destDir string) error {
	img, err := nativeImageForSource(ctx, source)
	if err != nil {
		return err
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("native export failed to get layers: %w", err)
	}

	var (
		downloadedBytes int64
		completedLayers int32
		totalBytes      = source.CompressedSizeBytes
		totalLayers     = len(layers)

		progressMu sync.Mutex
		lastTime   time.Time
		lastBytes  int64
	)

	reportProgress := func(force bool) {
		if source.OnPullProgress == nil {
			return
		}

		progressMu.Lock()
		defer progressMu.Unlock()

		now := time.Now()
		if !force && !lastTime.IsZero() && now.Sub(lastTime) < 800*time.Millisecond {
			return
		}

		downloaded := atomic.LoadInt64(&downloadedBytes)
		completed := int(atomic.LoadInt32(&completedLayers))

		var speed int64
		if !lastTime.IsZero() {
			dt := now.Sub(lastTime).Seconds()
			if dt > 0 {
				speed = int64(float64(downloaded-lastBytes) / dt)
			}
		}

		var percent float64
		if totalBytes > 0 {
			percent = float64(downloaded) / float64(totalBytes) * 100
			if percent > 100 {
				percent = 100
			}
		} else if totalLayers > 0 {
			percent = float64(completed) / float64(totalLayers) * 100
		}

		source.OnPullProgress(PullProgress{
			TotalBytes:      totalBytes,
			DownloadedBytes: downloaded,
			TotalLayers:     totalLayers,
			CompletedLayers: completed,
			SpeedBPS:        speed,
			Percent:         percent,
		})

		lastTime = now
		lastBytes = downloaded
	}

	reportProgress(true)

	// Step 1: Concurrently prefetch layers to disk to maximize network throughput.
	// Temp directory is created in destDir's workspace to utilize the same fast disk.
	prefetchDir, err := os.MkdirTemp(filepath.Dir(destDir), "native-prefetch-*")
	if err != nil {
		return fmt.Errorf("native export failed to create prefetch dir: %w", err)
	}
	defer os.RemoveAll(prefetchDir)

	jobs := nativeExportConcurrency()
	sem := make(chan struct{}, jobs)

	type layerFetch struct {
		path string
		err  error
		done chan struct{}
	}
	fetches := make([]*layerFetch, len(layers))
	for i := range fetches {
		fetches[i] = &layerFetch{done: make(chan struct{})}
	}

	eg, egCtx := errgroup.WithContext(ctx)

	// Schedule concurrent downloads.
	for i, l := range layers {
		layerIdx := i
		layer := l

		eg.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-egCtx.Done():
				return egCtx.Err()
			}
			defer func() { <-sem }()

			rc, err := layer.Compressed()
			if err != nil {
				err = fmt.Errorf("failed to open compressed stream for layer %d: %w", layerIdx, err)
				fetches[layerIdx].err = err
				close(fetches[layerIdx].done)
				return err
			}
			var closeOnce sync.Once
			closeRC := func() { closeOnce.Do(func() { _ = rc.Close() }) }
			defer closeRC()

			// Bridge context to explicitly abort network read if context cancels early.
			stopWatch := context.AfterFunc(egCtx, closeRC)
			defer stopWatch()

			f, err := os.CreateTemp(prefetchDir, fmt.Sprintf("layer-%03d-*.tar", layerIdx))
			if err != nil {
				err = fmt.Errorf("failed to create temp file for layer %d: %w", layerIdx, err)
				fetches[layerIdx].err = err
				close(fetches[layerIdx].done)
				return err
			}
			path := f.Name()

			buf := nativeCopyBufferPool.Get().([]byte)
			defer nativeCopyBufferPool.Put(buf)

			pr := &progressReader{
				Reader: rc,
				onRead: func(n int) {
					atomic.AddInt64(&downloadedBytes, int64(n))
					reportProgress(false)
				},
			}

			if _, err := io.CopyBuffer(f, pr, buf); err != nil {
				_ = f.Close()
				if ctxErr := egCtx.Err(); ctxErr != nil {
					err = fmt.Errorf("failed to download layer %d: %v (context canceled: %w)", layerIdx, err, ctxErr)
				} else {
					err = fmt.Errorf("failed to download layer %d: %w", layerIdx, err)
				}
				fetches[layerIdx].err = err
				close(fetches[layerIdx].done)
				return err
			}

			if err := f.Close(); err != nil {
				err = fmt.Errorf("failed to close temp file for layer %d: %w", layerIdx, err)
				fetches[layerIdx].err = err
				close(fetches[layerIdx].done)
				return err
			}

			atomic.AddInt32(&completedLayers, 1)
			reportProgress(true)

			fetches[layerIdx].path = path
			close(fetches[layerIdx].done)
			return nil
		})
	}

	// Step 2: Sequentially apply and immediately delete layers as they finish downloading.
	eg.Go(func() error {
		var br *bufio.Reader

		for i := 0; i < len(layers); i++ {
			select {
			case <-egCtx.Done():
				return egCtx.Err()
			case <-fetches[i].done:
			}

			if fetches[i].err != nil {
				return fetches[i].err
			}

			path := fetches[i].path
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("native export failed to open prefetched layer %d: %w", i, err)
			}

			// Reuse the same large buffer across all layers since extraction is strictly sequential.
			if br == nil {
				br = bufio.NewReaderSize(f, nativeCopyBufferSize)
			} else {
				br.Reset(f)
			}

			decompressed, err := compression.DecompressStream(br)
			if err != nil {
				_ = f.Close()
				return fmt.Errorf("native export failed to decompress layer %d: %w", i, err)
			}
			// WithNoSameOwner,it squashes all uid/gid to the
			// unpacking user (root), breaking images with non-root-owned files.
			_, applyErr := archive.Apply(egCtx, destDir, decompressed)
			_ = decompressed.Close()
			_ = f.Close() // safe to double close, ensures FD is freed immediately

			if applyErr != nil {
				return fmt.Errorf("native export failed to apply layer %d to %q (Hint: this might require root privileges, CAP_MKNOD, or a destination filesystem that supports xattrs/capabilities): %w", i, destDir, applyErr)
			}

			// TODO: Cache layers securely per-tenant.
			// Delete temp file immediately to save disk space.
			_ = os.Remove(path)
		}
		return nil
	})

	return eg.Wait()
}

func nativeImageForSource(ctx context.Context, source *PreparedSource) (v1.Image, error) {
	if source == nil {
		return nil, fmt.Errorf("native export source is nil")
	}
	if source.nativeImage != nil {
		return source.nativeImage, nil
	}
	return nil, fmt.Errorf("native export source image was not prepared")
}

func nativeExportConcurrency() int {
	raw := os.Getenv(nativeExportJobsEnv)
	if raw == "" {
		return defaultNativeExportConcurrency
	}
	jobs, err := strconv.Atoi(raw)
	if err != nil || jobs <= 0 {
		return defaultNativeExportConcurrency
	}
	if jobs > maxNativeExportConcurrency {
		return maxNativeExportConcurrency
	}
	return jobs
}

func defaultPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}
