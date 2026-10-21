// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

var pmemFileLocks = utils.NewResourceLocks()

func EnsurePmemFile(ctx context.Context, instanceType, imageRef string) error {
	if err := EnsurePmemRootfs(ctx, instanceType, imageRef); err != nil {
		return err
	}
	return ensureArtifactRuntimeFiles(ctx, instanceType, imageRef)
}

// EnsurePmemRootfs ensures the ext4 rootfs artifact exists locally.
func EnsurePmemRootfs(ctx context.Context, instanceType, imageRef string) error {
	if instanceType == "" || imageRef == "" {
		return fmt.Errorf("instanceType or imageRef is empty")
	}
	if err := pathutil.ValidateSafeID(instanceType); err != nil {
		return fmt.Errorf("invalid instanceType: %w", err)
	}
	if err := pathutil.ValidateSafeID(imageRef); err != nil {
		return fmt.Errorf("invalid imageRef: %w", err)
	}
	imagePath := pmem.GetRawImageFilePath(instanceType, imageRef)
	// CreateImage and node-distribution requests may legitimately reuse the
	// same artifact. Lock at this shared entry point so every writer of the
	// fixed .download path is serialized and waiters recheck the final file.
	unlock, err := pmemFileLocks.LockContext(ctx, imagePath)
	if err != nil {
		return err
	}
	defer unlock()

	exist, err := utils.FileExistAndValid(imagePath)
	if err != nil {
		log.G(ctx).Warnf("pmem file %s validation failed, try download: %v", imagePath, err)
	}
	if !exist {
		spec := constants.GetImageSpec(ctx)
		if spec == nil {
			return fmt.Errorf("pmem file %s not exist", imagePath)
		}
		if err := tryDownloadPmemFile(ctx, imagePath, spec); err != nil {
			return fmt.Errorf("pmem file %s not exist and download failed: %v", imagePath, err)
		}
		exist, err = utils.FileExistAndValid(imagePath)
		if err != nil {
			return fmt.Errorf("downloaded pmem file %s validation failed: %v", imagePath, err)
		}
		if !exist {
			return fmt.Errorf("downloaded pmem file %s not exist", imagePath)
		}
	}
	return nil
}

func tryDownloadPmemFile(ctx context.Context, imagePath string, spec *cubeimages.ImageSpec) error {
	if spec == nil || spec.Annotations == nil {
		return fmt.Errorf("image spec annotations are empty")
	}
	downloadURL := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactURL])
	if downloadURL == "" {
		return fmt.Errorf("artifact download url is empty")
	}
	downloadURL = rewriteDownloadHost(downloadURL, cubeMasterHTTPAddr())
	expectedSHA := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactSHA256])
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return err
	}
	tmpPath := imagePath + ".download"
	// Resume support: a previous attempt may have left a partial .download
	// behind. tmpPath is derived from imagePath (artifact fingerprint), so
	// any leftover partial belongs to this same artifact and can be safely
	// resumed instead of restarting the transfer from zero, which is
	// expensive on slow or metered links.
	resumeFrom := int64(0)
	havePartial := false
	if st, err := os.Stat(tmpPath); err == nil && st.Size() > 0 {
		resumeFrom = st.Size()
		havePartial = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	if havePartial {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}
	if token := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactToken]); token != "" {
		req.Header.Set("X-Cube-Artifact-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	verifyOnly := false
	appendMode := false
	switch {
	case havePartial && resp.StatusCode == http.StatusPartialContent:
		// Server honored the range request; append to the partial file.
		appendMode = true
	case havePartial && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// The partial file already reaches EOF: the previous attempt most
		// likely died between download completion and rename. Skip the
		// transfer and validate what is already on disk.
		verifyOnly = true
	case resp.StatusCode == http.StatusOK:
		// Full-body response: the normal no-partial path, or the fallback
		// when the server ignores Range. Discard any partial and restart.
	default:
		return fmt.Errorf("download status code %d", resp.StatusCode)
	}

	hasher := sha256.New()
	if !verifyOnly {
		flag := os.O_WRONLY | os.O_CREATE
		if !appendMode {
			// Full-body path (fresh download, or the fallback when the
			// server ignores Range): discard any stale partial first.
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(tmpPath, flag, 0o666) // NOCC:Path Traversal()
		if err != nil {
			return err
		}
		startOffset := int64(0)
		if appendMode {
			// Continue writing at the partial's end; sparseCopy seeks
			// relative to the current position for zero runs.
			if _, err := f.Seek(resumeFrom, io.SeekStart); err != nil {
				_ = f.Close()
				return err
			}
			startOffset = resumeFrom
		}
		const sparseBufSize = 1024 * 1024 // 1 MiB
		if _, err := sparseCopy(f, hasher, resp.Body, sparseBufSize, startOffset); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if appendMode || verifyOnly {
		// The streaming hash state cannot span the resume boundary;
		// re-hash the complete file from disk instead.
		pf, err := os.Open(tmpPath) // NOCC:Path Traversal()
		if err != nil {
			return err
		}
		hasher.Reset()
		_, err = io.Copy(hasher, pf)
		_ = pf.Close()
		if err != nil {
			return err
		}
	}
	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA != "" {
		if !strings.EqualFold(gotSHA, expectedSHA) {
			// A corrupt partial has no salvage value; drop it so the next
			// attempt starts clean.
			_ = os.Remove(tmpPath)
			return fmt.Errorf("artifact sha256 mismatch, got %s want %s", gotSHA, expectedSHA)
		}
	}
	if err := os.Rename(tmpPath, imagePath); err != nil {
		return err
	}
	return nil
}

// isAllZeros checks if the byte slice consists entirely of zero bytes.
func isAllZeros(buf []byte) bool {
	for len(buf) >= 8 {
		if binary.LittleEndian.Uint64(buf) != 0 {
			return false
		}
		buf = buf[8:]
	}
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

const sparseHoleThreshold = int64(1024 * 1024)

// sparseCopy copies from src to dst while leaving zero runs of at least 1 MiB
// sparse. Zero runs may start at any offset and may cross read-buffer boundaries.
// It simultaneously feeds all read bytes into hasher (if non-nil) to preserve
// hash integrity. dst.Truncate is called at the end to guarantee the file's
// apparent size matches total bytes read.
func sparseCopy(dst *os.File, hasher hash.Hash, src io.Reader, bufSize int, startOffset int64) (int64, error) {
	if bufSize <= 0 {
		bufSize = 1024 * 1024
	}
	if startOffset > 0 {
		if _, err := dst.Seek(startOffset, io.SeekStart); err != nil {
			return 0, err
		}
	}
	buf := make([]byte, bufSize)
	zeroBuf := make([]byte, min(bufSize, int(sparseHoleThreshold)))
	var totalBytes int64
	var pendingZeros int64

	flushZeros := func() error {
		if pendingZeros == 0 {
			return nil
		}
		if pendingZeros >= sparseHoleThreshold {
			_, err := dst.Seek(pendingZeros, io.SeekCurrent)
			pendingZeros = 0
			return err
		}
		remaining := pendingZeros
		for remaining > 0 {
			n := min(int64(len(zeroBuf)), remaining)
			if _, err := dst.Write(zeroBuf[:n]); err != nil {
				return err
			}
			remaining -= n
		}
		pendingZeros = 0
		return nil
	}

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if hasher != nil {
				if _, err := hasher.Write(chunk); err != nil {
					return totalBytes, err
				}
			}
			if isAllZeros(chunk) {
				pendingZeros += int64(n)
			} else {
				for offset := 0; offset < len(chunk); {
					if chunk[offset] == 0 {
						start := offset
						for offset < len(chunk) && chunk[offset] == 0 {
							offset++
						}
						pendingZeros += int64(offset - start)
						continue
					}
					if err := flushZeros(); err != nil {
						return totalBytes, err
					}
					start := offset
					if nextZero := bytes.IndexByte(chunk[offset:], 0); nextZero >= 0 {
						offset += nextZero
					} else {
						offset = len(chunk)
					}
					if _, err := dst.Write(chunk[start:offset]); err != nil {
						return totalBytes, err
					}
				}
			}
			totalBytes += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return totalBytes, readErr
		}
	}
	if err := flushZeros(); err != nil {
		return totalBytes, err
	}
	if err := dst.Truncate(startOffset + totalBytes); err != nil {
		return totalBytes, err
	}
	return totalBytes, nil
}

// RefreshArtifactRuntimeFiles rewrites runtime companion files from the current shared sources.
func RefreshArtifactRuntimeFiles(ctx context.Context, instanceType, imageRef string) error {
	return refreshKernelFile(ctx, instanceType, imageRef)
}

func validateArtifactRuntimeFilesPresent(ctx context.Context, instanceType, imageRef string) error {
	return ensureKernelFilePresent(ctx, instanceType, imageRef)
}

func ensureArtifactRuntimeFiles(ctx context.Context, instanceType, imageRef string) error {
	if err := ensureKernelFilePresent(ctx, instanceType, imageRef); err != nil {
		log.G(ctx).Warnf("artifact kernel file validation failed, refreshing from shared kernel: %v", err)
		if refreshErr := refreshKernelFile(ctx, instanceType, imageRef); refreshErr != nil {
			return fmt.Errorf("refresh artifact kernel file failed: %w", refreshErr)
		}
	}
	return nil
}

func ensureKernelFilePresent(ctx context.Context, instanceType, imageRef string) error {
	return pmem.EnsureKernelFilePresent(ctx, pmem.GetSharedKernelFilePath(), pmem.GetRawKernelFilePath(instanceType, imageRef))
}

func refreshKernelFile(ctx context.Context, instanceType, imageRef string) error {
	return pmem.RefreshKernelFile(ctx, pmem.GetSharedKernelFilePath(), pmem.GetRawKernelFilePath(instanceType, imageRef))
}

// isS3PresignedURL reports whether rawURL is an S3/MinIO presigned URL
// (AWS SigV4), identified by its X-Amz-Signature query parameter. TC's
// s3store.PresignedGetURL always signs with SigV4, so this covers every
// S3-backed artifact URL CubeMaster can hand out.
func isS3PresignedURL(u *url.URL) bool {
	return u.Query().Has("X-Amz-Signature")
}

// cubeMasterHTTPAddr returns the locally configured CubeMaster HTTP address
// used to rewrite CubeMaster's own download route, or "" when unset/unset up
// (rewriteDownloadHost then leaves the URL untouched).
func cubeMasterHTTPAddr() string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.MetaServerConfig == nil {
		return ""
	}
	return strings.TrimSpace(cfg.MetaServerConfig.CubeMasterHTTPAddr)
}

// rewriteDownloadHost swaps rawURL's host for endpoint (the locally
// configured CubeMaster HTTP address). This exists because the artifact
// row's MasterNodeIP (used to build CubeMaster's own /cube/template/...
// download route) may not be reachable from every cubelet's network, while
// the cubelet-local cubemaster_http_addr config always is.
//
// S3/MinIO presigned URLs (artifact.ArtifactURL, set when TC uploads the
// artifact to S3) must be passed through unchanged: the signature is only
// valid for the original host, and CubeMaster has no route matching the S3
// object path/query anyway -- rewriting the host here used to silently turn
// a valid presigned URL into a 404 (Error: "download status code 404").
func rewriteDownloadHost(rawURL, endpoint string) string {
	if endpoint == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if isS3PresignedURL(u) {
		return rawURL
	}
	u.Host = endpoint
	return u.String()
}
