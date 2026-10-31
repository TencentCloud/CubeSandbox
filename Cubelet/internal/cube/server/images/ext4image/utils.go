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
	"strconv"
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
	release, err := pmem.PinArtifact(ctx, instanceType, imageRef)
	if err != nil {
		return err
	}
	defer release()
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
	release, pinErr := pmem.PinArtifact(ctx, instanceType, imageRef)
	if pinErr != nil {
		return pinErr
	}
	defer release()
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
		if err := tryDownloadPmemFile(ctx, instanceType, imagePath, spec); err != nil {
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
	if err := recordCacheUse(ctx, instanceType, imageRef); err != nil {
		// Missing source information prevents GC, not legacy template use.
		log.G(ctx).Warnf("record rootfs cache use for %s failed: %v", imageRef, err)
	}
	return nil
}

func tryDownloadPmemFile(ctx context.Context, instanceType, imagePath string, spec *cubeimages.ImageSpec) error {
	annotations := spec.GetAnnotations()
	downloadURL := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactURL])
	expectedSHA := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactSHA256])
	expectedSize, _ := strconv.ParseInt(annotations[constants.MasterAnnotationRootfsArtifactSizeBytes], 10, 64)
	token := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactToken])
	var source *CacheSource
	var err error
	if instanceType == "cubebox" {
		source, err = sourceFromSpec(instanceType, filepath.Base(filepath.Dir(imagePath)), spec)
	}
	if err != nil {
		// Recovery metadata controls eligibility for GC, not legacy downloads.
		log.G(ctx).Warnf("rootfs cache source is not eligible for GC: %v", err)
		source = nil
	}
	if instanceType == "cubebox" && source == nil && annotations[constants.MasterAnnotationRootfsArtifactCacheable] != "false" {
		if entry, err := readCacheEntry(imagePath); err == nil {
			source = &entry.CacheSource
		}
	}
	if source != nil {
		downloadURL, expectedSHA, expectedSize, token = source.downloadURL(), source.SHA256, source.SizeBytes, source.Token
	}
	if downloadURL == "" {
		return fmt.Errorf("artifact download url is empty")
	}
	downloadURL = rewriteDownloadHost(downloadURL, cubeMasterHTTPAddr())
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return err
	}
	tmpPath := imagePath + ".download"
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) { // NOCC:Path Traversal()
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("invalid artifact download request")
	}
	if token != "" {
		req.Header.Set("X-Cube-Artifact-Token", token)
	}
	// Reserve before opening the response body; a pressure sweep can take
	// seconds. Active GC requires the declared artifact size up front.
	if instanceType == "cubebox" {
		releaseSpace, err := reserveCacheDownload(ctx, expectedSize)
		if err != nil {
			return err
		}
		defer releaseSpace()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("artifact download request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download status code %d", resp.StatusCode)
	}
	if expectedSize <= 0 {
		expectedSize = resp.ContentLength
	}
	if expectedSize > 0 && resp.ContentLength >= 0 && resp.ContentLength != expectedSize {
		return fmt.Errorf("artifact content length does not match expected size")
	}
	f, err := os.Create(tmpPath) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer f.Close()
	published := false
	defer func() {
		if !published {
			if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
				log.G(ctx).Warnf("remove incomplete rootfs artifact %s failed: %v", tmpPath, err)
			}
		}
	}()
	hasher := sha256.New()
	var reader io.Reader = resp.Body
	if expectedSize > 0 {
		reader = io.LimitReader(reader, expectedSize+1)
	}
	if _, err := sparseCopy(f, hasher, reader, 1024*1024); err != nil {
		return err
	}
	if expectedSize > 0 {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() != expectedSize {
			return fmt.Errorf("artifact size mismatch")
		}
	}
	if expectedSHA != "" {
		gotSHA := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(gotSHA, expectedSHA) {
			return fmt.Errorf("artifact sha256 mismatch, got %s want %s", gotSHA, expectedSHA)
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, imagePath); err != nil {
		return err
	}
	published = true
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
func sparseCopy(dst *os.File, hasher hash.Hash, src io.Reader, bufSize int) (int64, error) {
	if bufSize <= 0 {
		bufSize = 1024 * 1024
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
	if err := dst.Truncate(totalBytes); err != nil {
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
	if strings.Contains(endpoint, "://") {
		target, err := url.Parse(endpoint)
		if err != nil || target.Host == "" {
			return rawURL
		}
		u.Host, u.Scheme = target.Host, target.Scheme
	} else {
		u.Host = endpoint
	}
	return u.String()
}
