// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

var (
	pmemFileLocks              = utils.NewResourceLocks()
	artifactDownloadHTTPClient = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many artifact download redirects")
			}
			if len(via) == 0 {
				return nil
			}
			from, fromErr := artifactticket.URLOrigin(via[0].URL.String())
			to, toErr := artifactticket.URLOrigin(req.URL.String())
			if fromErr != nil || toErr != nil || from != to {
				return errors.New("artifact download redirect changed origin")
			}
			return nil
		},
	}
)

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
	expectedSHA := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactSHA256])
	expectedSize, err := strconv.ParseInt(strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactSizeBytes]), 10, 64)
	if err != nil {
		expectedSize = 0 // Legacy annotations predate size_bytes.
	}
	resolver, err := artifactDownloadResolverForSpec(spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return err
	}
	tmpPath := imagePath + ".download"
	if expectedSize <= 0 {
		_ = os.Remove(tmpPath) // NOCC:Path Traversal()
	}
	resolvedSHA, resolvedSize, err := downloadArtifactWithResume(ctx, tmpPath, resolver, expectedSHA, expectedSize)
	if err != nil {
		return err
	}
	if expectedSHA == "" {
		expectedSHA = resolvedSHA
	}
	if expectedSize <= 0 {
		expectedSize = resolvedSize
	}
	if expectedSize > 0 {
		info, err := os.Stat(tmpPath) // NOCC:Path Traversal()
		if err != nil {
			return err
		}
		if info.Size() != expectedSize {
			return fmt.Errorf("artifact size mismatch, got %d want %d", info.Size(), expectedSize)
		}
	}
	if expectedSHA != "" {
		gotSHA, err := fileSHA256(tmpPath)
		if err != nil {
			return err
		}
		if !strings.EqualFold(gotSHA, expectedSHA) {
			_ = os.Remove(tmpPath) // Do not resume a complete but corrupt file.
			return fmt.Errorf("artifact sha256 mismatch, got %s want %s", gotSHA, expectedSHA)
		}
	}
	if err := os.Rename(tmpPath, imagePath); err != nil {
		return err
	}
	return nil
}

type resolvedArtifactDownload struct {
	URL               string
	Method            string
	Header            http.Header
	SizeBytes         int64
	SHA256            string
	EnforceSameOrigin bool
}

type artifactDownloadResolver func(context.Context) (*resolvedArtifactDownload, error)

type artifactDownloadClass int

const (
	artifactDownloadRetryable artifactDownloadClass = iota
	artifactDownloadExpired
	artifactDownloadFatal
)

type artifactDownloadError struct {
	err   error
	class artifactDownloadClass
}

func (e *artifactDownloadError) Error() string {
	if e == nil || e.err == nil {
		return "artifact download failed"
	}
	return e.err.Error()
}

func (e *artifactDownloadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func isRetryableArtifactDownloadError(err error) bool {
	if err == nil {
		return false
	}
	var classified *artifactDownloadError
	if errors.As(err, &classified) {
		return classified.class != artifactDownloadFatal
	}
	return true
}

// artifactDownloadNodeID resolves the node identity bound into download
// tickets. It is a variable so tests can run without host metadata.
var artifactDownloadNodeID = utils.GetInstanceID

// artifactDownloadResolverForSpec only supports the ticket protocol: Cubelet
// obtains a short-lived ticket from CubeMaster and then fetches the bytes
// directly from S3 or the owning TemplateCenter. Legacy annotations that
// point at a CubeMaster-proxied byte stream are rejected.
func artifactDownloadResolverForSpec(spec *cubeimages.ImageSpec) (artifactDownloadResolver, error) {
	annotations := spec.Annotations
	ticketURL := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactTicketURL])
	if ticketURL == "" {
		if strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactURL]) != "" {
			return nil, &artifactDownloadError{
				err:   errors.New("legacy Master-proxied artifact download is not supported; recreate the sandbox to receive a direct-download ticket"),
				class: artifactDownloadFatal,
			}
		}
		return nil, fmt.Errorf("artifact download ticket url is empty")
	}
	nodeID, err := artifactDownloadNodeID()
	if err != nil {
		return nil, fmt.Errorf("resolve node id for artifact ticket: %w", err)
	}
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("resolve node id for artifact ticket: empty node id")
	}
	ticketURL = rewriteDownloadHost(ticketURL, cubeMasterHTTPAddr())
	artifactID := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactID])
	refreshToken := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactRefreshToken])
	expectedOrigin := strings.TrimSpace(annotations[constants.MasterAnnotationRootfsArtifactDownloadOrigin])
	if refreshToken == "" || expectedOrigin == "" {
		return nil, fmt.Errorf("artifact ticket refresh authorization is incomplete")
	}
	return func(ctx context.Context) (*resolvedArtifactDownload, error) {
		return fetchArtifactDownloadTicket(ctx, ticketURL, artifactID, nodeID, refreshToken, expectedOrigin)
	}, nil
}

func fetchArtifactDownloadTicket(ctx context.Context, ticketURL, artifactID, nodeID, refreshToken, expectedOrigin string) (*resolvedArtifactDownload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ticketURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(constants.ArtifactDownloadNodeIDHeader, nodeID)
	req.Header.Set(constants.ArtifactDownloadRefreshHeader, refreshToken)
	resp, err := artifactDownloadHTTPClient.Do(req)
	if err != nil {
		return nil, sanitizeArtifactHTTPError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("artifact ticket status code %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &artifactDownloadError{err: err, class: artifactDownloadFatal}
		}
		if resp.StatusCode >= 500 {
			return nil, &artifactDownloadError{err: err, class: artifactDownloadRetryable}
		}
		return nil, err
	}
	var ticket artifactticket.Ticket
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&ticket); err != nil {
		return nil, fmt.Errorf("decode artifact download ticket: %w", err)
	}
	// Compare expiry to the issuer's clock, not the node's. The target
	// (TC/S3) remains authoritative and will reject an actually expired
	// capability; using the Cubelet clock here would turn ordinary node clock
	// skew into a permanent download failure.
	if ticket.ArtifactID != artifactID || ticket.Generation <= 0 ||
		ticket.ServerTime.IsZero() || !ticket.ExpiresAt.After(ticket.ServerTime) {
		return nil, fmt.Errorf("artifact download ticket identity or expiry is invalid")
	}
	u, err := url.Parse(strings.TrimSpace(ticket.URL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("artifact download ticket contains an invalid URL")
	}
	if ticket.Source != artifactticket.SourceS3 && ticket.Source != artifactticket.SourceTemplateCenter {
		return nil, &artifactDownloadError{
			err:   fmt.Errorf("artifact download ticket source %q is not a direct source", ticket.Source),
			class: artifactDownloadFatal,
		}
	}
	origin, err := artifactticket.URLOrigin(ticket.URL)
	if err != nil || origin != expectedOrigin {
		return nil, fmt.Errorf("artifact download ticket origin does not match the authorized origin")
	}
	method, header, err := ticket.DownloadRequest(nodeID)
	if err != nil {
		return nil, &artifactDownloadError{err: err, class: artifactDownloadFatal}
	}
	return &resolvedArtifactDownload{
		URL:               ticket.URL,
		Method:            method,
		Header:            header,
		SizeBytes:         ticket.SizeBytes,
		SHA256:            ticket.SHA256,
		EnforceSameOrigin: true,
	}, nil
}

func downloadArtifactWithResume(ctx context.Context, tmpPath string, resolve artifactDownloadResolver, expectedSHA string, expectedSize int64) (string, int64, error) {
	const maxAttempts = 4
	var lastErr error
	var resolvedSHA string
	var resolvedSize int64
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", 0, ctx.Err()
			case <-timer.C:
			}
		}
		source, err := resolve(ctx)
		if err != nil {
			lastErr = err
			if !isRetryableArtifactDownloadError(err) {
				break
			}
			continue
		}
		if expectedSize > 0 && source.SizeBytes > 0 && source.SizeBytes != expectedSize {
			return "", 0, fmt.Errorf("ticket artifact size %d does not match annotation %d", source.SizeBytes, expectedSize)
		}
		if expectedSHA != "" && source.SHA256 != "" && !strings.EqualFold(source.SHA256, expectedSHA) {
			return "", 0, fmt.Errorf("ticket artifact sha256 does not match annotation")
		}
		if source.SizeBytes > 0 {
			resolvedSize = source.SizeBytes
		}
		if source.SHA256 != "" {
			resolvedSHA = source.SHA256
		}
		targetSize := expectedSize
		if targetSize <= 0 {
			targetSize = resolvedSize
		}
		done, err := downloadArtifactAttempt(ctx, tmpPath, source, targetSize)
		if err == nil && done {
			return resolvedSHA, resolvedSize, nil
		}
		lastErr = err
		if err != nil && !isRetryableArtifactDownloadError(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("artifact download did not complete")
	}
	return "", 0, lastErr
}

func downloadArtifactAttempt(ctx context.Context, tmpPath string, source *resolvedArtifactDownload, expectedSize int64) (bool, error) {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR, 0o644) // NOCC:Path Traversal()
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	offset := info.Size()
	if expectedSize > 0 && offset == expectedSize {
		return true, nil
	}
	if expectedSize > 0 && offset > expectedSize {
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		offset = 0
	}
	method := source.Method
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet {
		return false, &artifactDownloadError{err: errors.New("artifact download method must be GET"), class: artifactDownloadFatal}
	}
	req, err := http.NewRequestWithContext(ctx, method, source.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header = source.Header.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := artifactDownloadHTTPClient.Do(req)
	if err != nil {
		return false, sanitizeArtifactHTTPError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if expectedSize > 0 && offset == expectedSize {
			return true, nil
		}
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		return false, fmt.Errorf("artifact server rejected resume offset %d", offset)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		err := fmt.Errorf("download status code %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			// TC ticket expired. The next resolve() issues a fresh ticket.
			return false, &artifactDownloadError{err: err, class: artifactDownloadExpired}
		case http.StatusForbidden:
			// S3 expired signatures commonly surface as 403. Re-ticket once;
			// a second 403 is treated as a permanent authorization failure.
			return false, &artifactDownloadError{err: err, class: artifactDownloadExpired}
		case http.StatusNotFound, http.StatusGone:
			return false, &artifactDownloadError{err: err, class: artifactDownloadFatal}
		default:
			if resp.StatusCode >= 500 {
				return false, &artifactDownloadError{err: err, class: artifactDownloadRetryable}
			}
			return false, err
		}
	}
	if resp.StatusCode == http.StatusPartialContent {
		if err := validateArtifactContentRange(resp, offset, expectedSize); err != nil {
			return false, err
		}
	}
	if offset > 0 && resp.StatusCode == http.StatusOK {
		// Server ignored Range; restart safely using this full response.
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	reader := io.Reader(resp.Body)
	remaining := int64(0)
	if expectedSize > 0 {
		remaining = expectedSize - offset
		if remaining < 0 {
			return false, fmt.Errorf("artifact resume offset %d exceeds expected size %d", offset, expectedSize)
		}
		if resp.ContentLength > remaining {
			return false, fmt.Errorf("artifact response length %d exceeds remaining size %d", resp.ContentLength, remaining)
		}
		reader = io.LimitReader(resp.Body, remaining+1)
	}
	written, err := io.Copy(f, reader)
	if err != nil {
		return false, err
	}
	if expectedSize > 0 && written > remaining {
		return false, fmt.Errorf("artifact response exceeds remaining size %d", remaining)
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	info, err = f.Stat()
	if err != nil {
		return false, err
	}
	if expectedSize > 0 {
		if info.Size() > expectedSize {
			return false, fmt.Errorf("downloaded artifact exceeds expected size: got %d want %d", info.Size(), expectedSize)
		}
		if info.Size() == expectedSize {
			return true, nil
		}
		return false, fmt.Errorf("short artifact download: got %d want %d", info.Size(), expectedSize)
	}
	return true, nil
}

func validateArtifactContentRange(resp *http.Response, offset, expectedSize int64) error {
	raw := strings.TrimSpace(resp.Header.Get("Content-Range"))
	if !strings.HasPrefix(strings.ToLower(raw), "bytes ") {
		return errors.New("partial artifact response has no valid Content-Range")
	}
	value := strings.TrimSpace(raw[len("bytes "):])
	rangePart, totalPart, ok := strings.Cut(value, "/")
	if !ok {
		return fmt.Errorf("invalid artifact Content-Range %q", raw)
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return fmt.Errorf("invalid artifact Content-Range %q", raw)
	}
	start, startErr := strconv.ParseInt(startPart, 10, 64)
	end, endErr := strconv.ParseInt(endPart, 10, 64)
	if startErr != nil || endErr != nil || start != offset || end < start {
		return fmt.Errorf("artifact Content-Range %q does not match offset %d", raw, offset)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
		return fmt.Errorf("artifact Content-Range length does not match Content-Length")
	}
	if totalPart == "*" {
		if expectedSize > 0 {
			return fmt.Errorf("artifact Content-Range %q has no total size", raw)
		}
		return nil
	}
	total, err := strconv.ParseInt(totalPart, 10, 64)
	if err != nil || total <= end {
		return fmt.Errorf("invalid artifact Content-Range total %q", raw)
	}
	if expectedSize > 0 && total != expectedSize {
		return fmt.Errorf("artifact Content-Range total %d does not match expected size %d", total, expectedSize)
	}
	return nil
}

func sanitizeArtifactHTTPError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return errors.New("artifact HTTP request failed")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
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
// used to rewrite CubeMaster's ticket route, or "" when unset
// (rewriteDownloadHost then leaves the URL untouched).
func cubeMasterHTTPAddr() string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.MetaServerConfig == nil {
		return ""
	}
	return strings.TrimSpace(cfg.MetaServerConfig.CubeMasterHTTPAddr)
}

// rewriteDownloadHost swaps rawURL's host for endpoint (the locally
// configured CubeMaster HTTP address). This exists because the recorded
// Master address may not be reachable from every cubelet's network, while
// cubemaster_http_addr always is.
//
// Only CubeMaster's own ticket route is rewritten. TemplateCenter download
// URLs and S3/MinIO presigned URLs must keep the host they were issued for:
// SigV4 is bound to host, and a TC URL rewritten back to CubeMaster would put
// the ext4 bytes on the control plane.
func rewriteDownloadHost(rawURL, endpoint string) string {
	if endpoint == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if isS3PresignedURL(u) || !isCubeMasterTicketPath(u.Path) {
		return rawURL
	}
	u.Host = endpoint
	return u.String()
}

func isCubeMasterTicketPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.Contains(path, "/internal/template/artifacts/") &&
		strings.HasSuffix(path, "/download-ticket")
}
