// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultVolumeRefreshMargin = 2 * time.Minute
	defaultFileMode            = 0o644
	defaultDirMode             = 0o755
)

// VolumeFileType describes the kind of an entry inside a Volume.
type VolumeFileType string

const (
	VolumeFileTypeFile      VolumeFileType = "file"
	VolumeFileTypeDirectory VolumeFileType = "directory"
	VolumeFileTypeSymlink   VolumeFileType = "symlink"
	VolumeFileTypeUnknown   VolumeFileType = "unknown"
)

// VolumeEntry represents metadata for a file or directory inside a Volume.
type VolumeEntry struct {
	Name   string         `json:"name"`
	Type   VolumeFileType `json:"type"`
	Path   string         `json:"path"`
	Size   int64          `json:"size"`
	Mode   int            `json:"mode"`
	UID    int            `json:"uid"`
	GID    int            `json:"gid"`
	Atime  time.Time      `json:"atime"`
	Mtime  time.Time      `json:"mtime"`
	Ctime  time.Time      `json:"ctime"`
	Target string         `json:"target,omitempty"`
}

// IsDir reports whether the entry is a directory.
func (e VolumeEntry) IsDir() bool {
	return e.Type == VolumeFileTypeDirectory
}

// VolumeContentCredentials holds short-lived, plugin-issued storage credentials
// returned by POST /volumes/{volumeID}/content-token.
//
// String and GoString mask secret fields so credentials never leak into logs
// or formatted diagnostics.
type VolumeContentCredentials struct {
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	Prefix          string `json:"prefix"`
	AddressingStyle string `json:"addressing_style"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	// ExpiresAt is the Unix timestamp in seconds when these credentials expire.
	ExpiresAt int64 `json:"expires_at"`
}

// String masks sensitive credential material.
func (c VolumeContentCredentials) String() string {
	mask := func(s string) string {
		if s == "" {
			return ""
		}
		return "***"
	}
	return fmt.Sprintf(
		"VolumeContentCredentials{Provider:%q, Endpoint:%q, Bucket:%q, Region:%q, Prefix:%q, AddressingStyle:%q, AccessKeyID:%q, SecretAccessKey:%q, SessionToken:%q, ExpiresAt:%d}",
		c.Provider, c.Endpoint, c.Bucket, c.Region, c.Prefix, c.AddressingStyle,
		mask(c.AccessKeyID), mask(c.SecretAccessKey), mask(c.SessionToken), c.ExpiresAt,
	)
}

// GoString masks sensitive credential material for %#v formatting.
func (c VolumeContentCredentials) GoString() string {
	return c.String()
}

// VolumeListOptions configures VolumeContent.List.
type VolumeListOptions struct {
	// Depth controls traversal depth beneath path.
	// nil or 1 lists direct children; 0 returns an empty list; values > 1
	// traverse recursively up to that depth. Negative values are rejected.
	Depth *int
}

// VolumeWriteOptions configures VolumeContent.Write and VolumeContent.WriteFrom.
type VolumeWriteOptions struct {
	UID  *int
	GID  *int
	Mode *int
	// Force controls overwrite behavior. nil or true overwrites an existing
	// file; false fails with ErrVolumePathExists if the path already exists.
	Force *bool
}

// VolumeMakeDirOptions configures VolumeContent.MakeDir.
type VolumeMakeDirOptions struct {
	UID  *int
	GID  *int
	Mode *int
	// Force controls existing-directory behavior. nil or true succeeds
	// idempotently when the directory already exists; false fails with
	// ErrVolumePathExists if the directory already exists. Creating a directory
	// over an existing regular file always returns ErrVolumePathExists.
	Force *bool
}

// VolumeUpdateMetadataOptions configures VolumeContent.UpdateMetadata.
type VolumeUpdateMetadataOptions struct {
	UID  *int
	GID  *int
	Mode *int
}

// VolumeContentOption configures a VolumeContent handle created via Client.VolumeContent.
type VolumeContentOption func(*VolumeContent)

// WithVolumeRefreshMargin sets how far ahead of credential expiry the SDK
// proactively refreshes temporary storage credentials. Defaults to 2 minutes,
// automatically clamped for shorter-lived credentials.
func WithVolumeRefreshMargin(margin time.Duration) VolumeContentOption {
	return func(vc *VolumeContent) {
		if margin >= 0 {
			vc.refreshMargin = margin
		}
	}
}

// WithVolumeAllowedEndpointHosts restricts direct storage connections to the
// specified hostnames or domain suffixes (entries starting with "." match any
// subdomain, e.g. ".myqcloud.com").
func WithVolumeAllowedEndpointHosts(hosts ...string) VolumeContentOption {
	copied := append([]string(nil), hosts...)
	return func(vc *VolumeContent) {
		vc.policy.allowedEndpointHosts = copied
	}
}

// WithVolumeAllowInsecureLoopback explicitly permits HTTP and loopback storage
// endpoints for local integration/contract tests. Never enable this for
// untrusted endpoints.
func WithVolumeAllowInsecureLoopback(allow bool) VolumeContentOption {
	return func(vc *VolumeContent) {
		vc.policy.allowInsecureLoopback = allow
	}
}

// WithVolumeAllowPrivateEndpoints explicitly permits connecting to RFC1918/ULA
// private storage endpoints (for example, an in-VPC MinIO deployment). By
// default private, loopback, and link-local/metadata IPs are blocked to defend
// against SSRF.
func WithVolumeAllowPrivateEndpoints(allow bool) VolumeContentOption {
	return func(vc *VolumeContent) {
		vc.policy.allowPrivateEndpoints = allow
	}
}

// WithVolumeStorageHTTPClient injects a custom HTTP client for direct storage
// calls independently of the control-plane HTTP client. Redirects remain
// disabled on the cloned client.
func WithVolumeStorageHTTPClient(httpClient *http.Client) VolumeContentOption {
	return func(vc *VolumeContent) {
		vc.policy.httpClient = httpClient
	}
}

// withVolumeClock overrides the clock used for expiry checks and SigV4 signing (tests).
func withVolumeClock(clock func() time.Time) VolumeContentOption {
	return func(vc *VolumeContent) {
		if clock != nil {
			vc.clock = clock
			vc.policy.clock = clock
		}
	}
}

// withVolumeStorageFactory injects a storage constructor so control-plane and
// storage logic can be tested independently.
func withVolumeStorageFactory(factory func(VolumeContentCredentials) (volumeStorage, error)) VolumeContentOption {
	return func(vc *VolumeContent) {
		vc.storageFactory = factory
	}
}

// withVolumeMultipartChunkSize overrides the streaming multipart part size (tests).
func withVolumeMultipartChunkSize(size int) VolumeContentOption {
	return func(vc *VolumeContent) {
		vc.policy.partSize = size
	}
}

// IssueVolumeContentToken calls POST /volumes/{volumeID}/content-token using
// the client's authenticated control-plane transport and validates the returned
// short-lived storage credentials.
//
// The request sends no body and no caller identity: authorization, credential
// lifetime, prefix selection, and whether a session token is issued are owned
// by the volume plugin. This method requires a server/plugin supporting
// POST /volumes/{id}/content-token (PR #2297+).
func (c *Client) IssueVolumeContentToken(ctx context.Context, volumeID string) (*VolumeContentCredentials, error) {
	if c == nil {
		return nil, fmt.Errorf("client must not be nil")
	}
	return c.issueVolumeContentToken(ctx, volumeID, time.Now, s3StoragePolicy{
		allowInsecureLoopback: false,
		allowPrivateEndpoints: true, // structural validation here; dial-time SSRF policy is enforced on VolumeContent
	})
}

func (c *Client) issueVolumeContentToken(ctx context.Context, volumeID string, nowFn func() time.Time, policy s3StoragePolicy) (*VolumeContentCredentials, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/volumes/"+url.PathEscape(volumeID)+"/content-token", nil)
	if err != nil {
		return nil, err
	}

	httpClient := *c.controlHTTP
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("redirects are not allowed for volume content token requests")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("issue volume content credentials: %s", redactVolumeSecrets(err.Error(), c.config.APIKey))
	}
	defer resp.Body.Close()

	if !statusOK(resp.StatusCode, []int{http.StatusOK, http.StatusCreated}) {
		return nil, mapContentTokenError(resp, c.config.APIKey)
	}

	var creds VolumeContentCredentials
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(&creds); err != nil {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Kind:       apiErrorKindAPI,
			Message:    "invalid volume content credentials response",
		}
	}
	if err := validateVolumeContentCredentials(&creds, nowFn(), policy); err != nil {
		return nil, err
	}
	return &creds, nil
}

func mapContentTokenError(resp *http.Response, apiKey string) error {
	rawMsg := redactVolumeSecrets(readErrorMessage(resp), apiKey)
	apiErr := apiErrorFromStatus(resp.StatusCode, rawMsg)
	lower := strings.ToLower(apiErr.Message)

	switch {
	case resp.StatusCode == http.StatusNotImplemented:
		apiErr.Kind = apiErrorKindVolumeContentUnsupported
	case resp.StatusCode == http.StatusBadRequest && (strings.Contains(lower, "direct-access") || strings.Contains(lower, "content hook") || strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported")):
		apiErr.Kind = apiErrorKindVolumeContentUnsupported
	case resp.StatusCode == http.StatusNotFound && apiErr.Kind != apiErrorKindVolumeNotFound:
		// Older CubeAPI/CubeMaster deployments without /volumes/{id}/content-token
		// return a generic 404. Classify as unsupported rather than sandbox-not-found.
		apiErr.Kind = apiErrorKindVolumeContentUnsupported
	case resp.StatusCode == http.StatusMethodNotAllowed:
		apiErr.Kind = apiErrorKindVolumeContentUnsupported
	}
	return apiErr
}

// VolumeContent returns a direct storage content handle for volumeID without
// starting a helper sandbox. Short-lived credentials are fetched on demand via
// POST /volumes/{volumeID}/content-token and refreshed automatically before
// expiry. Direct storage errors never fall back silently to legacy HTTP
// endpoints or VolumeInfo.Token.
func (c *Client) VolumeContent(volumeID string, opts ...VolumeContentOption) (*VolumeContent, error) {
	if c == nil {
		return nil, fmt.Errorf("client must not be nil")
	}
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}
	vc := &VolumeContent{
		client:        c,
		volumeID:      volumeID,
		refreshMargin: defaultVolumeRefreshMargin,
		clock:         time.Now,
		policy: s3StoragePolicy{
			clock:    time.Now,
			partSize: defaultS3MultipartChunkSize,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(vc)
		}
	}
	if vc.storageFactory == nil {
		policy := vc.policy
		policy.clock = vc.clock
		vc.storageFactory = func(creds VolumeContentCredentials) (volumeStorage, error) {
			return newS3VolumeStorage(creds, policy)
		}
	}
	return vc, nil
}

// VolumeContent provides direct file and directory operations on a Volume
// backed by short-lived, plugin-issued S3/COS credentials.
type VolumeContent struct {
	client         *Client
	volumeID       string
	refreshMargin  time.Duration
	clock          func() time.Time
	policy         s3StoragePolicy
	storageFactory func(VolumeContentCredentials) (volumeStorage, error)

	mu            sync.Mutex
	cached        *volumeSession
	closed        bool
	credIssuedAt  time.Time
	credExpiresAt time.Time
}

type volumeSession struct {
	creds   VolumeContentCredentials
	storage volumeStorage
}

// VolumeID returns the volume identifier associated with this handle.
func (vc *VolumeContent) VolumeID() string {
	if vc == nil {
		return ""
	}
	return vc.volumeID
}

// Close releases cached credentials and closes idle storage connections.
func (vc *VolumeContent) Close() error {
	if vc == nil {
		return nil
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.closed = true
	if vc.cached != nil && vc.cached.storage != nil {
		err := vc.cached.storage.Close()
		vc.cached = nil
		return err
	}
	vc.cached = nil
	return nil
}

func (vc *VolumeContent) ensureSession(ctx context.Context, forceRefresh bool) (*volumeSession, error) {
	if vc == nil || vc.client == nil {
		return nil, fmt.Errorf("volume content is not attached to a client")
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()

	if vc.closed {
		return nil, fmt.Errorf("volume content client is closed")
	}

	now := vc.clock()
	if !forceRefresh && vc.cached != nil && !vc.needsRefreshLocked(now) {
		return vc.cached, nil
	}

	creds, err := vc.client.issueVolumeContentToken(ctx, vc.volumeID, vc.clock, vc.policy)
	if err != nil {
		return nil, err
	}
	storage, err := vc.storageFactory(*creds)
	if err != nil {
		return nil, err
	}
	if vc.cached != nil && vc.cached.storage != nil {
		_ = vc.cached.storage.Close()
	}
	session := &volumeSession{
		creds:   *creds,
		storage: storage,
	}
	vc.cached = session
	vc.credIssuedAt = vc.clock()
	vc.credExpiresAt = time.Unix(creds.ExpiresAt, 0).UTC()
	return session, nil
}

func (vc *VolumeContent) needsRefreshLocked(now time.Time) bool {
	if vc.cached == nil {
		return true
	}
	if !vc.credExpiresAt.After(now) {
		return true
	}
	margin := vc.refreshMargin
	lifetime := vc.credExpiresAt.Sub(vc.credIssuedAt)
	if lifetime > 0 && margin > lifetime/2 {
		margin = lifetime / 2
	}
	return !vc.credExpiresAt.After(now.Add(margin))
}

func (vc *VolumeContent) invalidateSession(target *volumeSession) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if vc.cached == target && target != nil {
		if target.storage != nil {
			_ = target.storage.Close()
		}
		vc.cached = nil
	}
}

// withSession runs an idempotent storage operation, refreshing once if the
// storage backend rejects the cached credentials as expired.
func (vc *VolumeContent) withSession(ctx context.Context, fn func(*volumeSession) error) error {
	sess, err := vc.ensureSession(ctx, false)
	if err != nil {
		return err
	}
	err = fn(sess)
	if err != nil && errors.Is(err, ErrVolumeCredentialsExpired) {
		vc.invalidateSession(sess)
		sess, err = vc.ensureSession(ctx, true)
		if err != nil {
			return err
		}
		return fn(sess)
	}
	return err
}

// List returns files and directories under path.
//
// Depth semantics:
//   - nil or 1: direct children of path.
//   - 0: empty slice (after validating path).
//   - > 1: recursive traversal up to Depth levels below path.
func (vc *VolumeContent) List(ctx context.Context, targetPath string, opts VolumeListOptions) ([]VolumeEntry, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	depth := 1
	if opts.Depth != nil {
		if *opts.Depth < 0 {
			return nil, fmt.Errorf("depth must be a non-negative integer, got %d", *opts.Depth)
		}
		depth = *opts.Depth
	}
	var entries []VolumeEntry
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		if depth == 0 {
			entries = []VolumeEntry{}
			return nil
		}
		listPrefix, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		if depth == 1 {
			entries, err = vc.listDepthOne(ctx, sess, listPrefix)
			return err
		}
		entries, err = vc.listRecursive(ctx, sess, norm, listPrefix, depth)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (vc *VolumeContent) listDepthOne(ctx context.Context, sess *volumeSession, listPrefix string) ([]VolumeEntry, error) {
	byPath := make(map[string]VolumeEntry)
	token := ""
	for {
		page, err := sess.storage.ListObjectsV2(ctx, listPrefix, "/", token, 0)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Objects {
			if obj.Key == listPrefix {
				continue
			}
			entryPath, err := objectKeyToVolumePath(sess.creds.Prefix, obj.Key)
			if err != nil {
				continue
			}
			if strings.HasSuffix(obj.Key, "/") {
				byPath[entryPath] = makeVolumeEntry(entryPath, VolumeFileTypeDirectory, 0, obj.LastModified, nil)
			} else {
				byPath[entryPath] = makeVolumeEntry(entryPath, VolumeFileTypeFile, obj.Size, obj.LastModified, nil)
			}
		}
		for _, cp := range page.CommonPrefixes {
			entryPath, err := objectKeyToVolumePath(sess.creds.Prefix, cp)
			if err != nil {
				continue
			}
			byPath[entryPath] = makeVolumeEntry(entryPath, VolumeFileTypeDirectory, 0, time.Time{}, nil)
		}
		if !page.IsTruncated {
			break
		}
		if page.NextContinuationToken == "" {
			return nil, &VolumeStorageError{Op: "list", Path: listPrefix, Message: "truncated list page missing continuation token"}
		}
		token = page.NextContinuationToken
	}
	return sortedVolumeEntries(byPath), nil
}

func (vc *VolumeContent) listRecursive(ctx context.Context, sess *volumeSession, basePath, listPrefix string, maxDepth int) ([]VolumeEntry, error) {
	byPath := make(map[string]VolumeEntry)
	token := ""
	for {
		page, err := sess.storage.ListObjectsV2(ctx, listPrefix, "", token, 0)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Objects {
			if obj.Key == listPrefix {
				continue
			}
			if !strings.HasPrefix(obj.Key, listPrefix) {
				continue
			}
			rel := strings.TrimPrefix(obj.Key, listPrefix)
			isDirMarker := strings.HasSuffix(rel, "/")
			relTrim := strings.TrimSuffix(rel, "/")
			if relTrim == "" {
				continue
			}
			segs := strings.Split(relTrim, "/")
			// Synthesize implicit parent directories up to maxDepth.
			parentLimit := len(segs) - 1
			if parentLimit > maxDepth {
				parentLimit = maxDepth
			}
			for i := 1; i <= parentLimit; i++ {
				parentPath := joinVolumePath(basePath, strings.Join(segs[:i], "/"))
				if _, exists := byPath[parentPath]; !exists {
					byPath[parentPath] = makeVolumeEntry(parentPath, VolumeFileTypeDirectory, 0, time.Time{}, nil)
				}
			}
			if len(segs) > maxDepth {
				continue
			}
			entryPath := joinVolumePath(basePath, relTrim)
			if isDirMarker {
				byPath[entryPath] = makeVolumeEntry(entryPath, VolumeFileTypeDirectory, 0, obj.LastModified, nil)
			} else {
				byPath[entryPath] = makeVolumeEntry(entryPath, VolumeFileTypeFile, obj.Size, obj.LastModified, nil)
			}
		}
		if !page.IsTruncated {
			break
		}
		if page.NextContinuationToken == "" {
			return nil, &VolumeStorageError{Op: "list", Path: listPrefix, Message: "truncated list page missing continuation token"}
		}
		token = page.NextContinuationToken
	}
	return sortedVolumeEntries(byPath), nil
}

// Stat returns metadata for a file or directory at path.
func (vc *VolumeContent) Stat(ctx context.Context, targetPath string) (*VolumeEntry, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	var entry VolumeEntry
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		if norm == "/" {
			entry = makeVolumeEntry("/", VolumeFileTypeDirectory, 0, vc.clock().UTC(), nil)
			return nil
		}
		fileKey, err := buildObjectKey(sess.creds.Prefix, norm, false)
		if err != nil {
			return err
		}
		if head, headErr := sess.storage.HeadObject(ctx, fileKey); headErr == nil {
			entry = makeVolumeEntry(norm, VolumeFileTypeFile, head.Size, head.LastModified, head.Metadata)
			return nil
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		dirKey, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		if head, headErr := sess.storage.HeadObject(ctx, dirKey); headErr == nil {
			entry = makeVolumeEntry(norm, VolumeFileTypeDirectory, 0, head.LastModified, head.Metadata)
			return nil
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		page, listErr := sess.storage.ListObjectsV2(ctx, dirKey, "", "", 1)
		if listErr != nil {
			return listErr
		}
		if page.KeyCount > 0 || len(page.Objects) > 0 || len(page.CommonPrefixes) > 0 {
			entry = makeVolumeEntry(norm, VolumeFileTypeDirectory, 0, vc.clock().UTC(), nil)
			return nil
		}
		return &VolumeStorageError{
			Op:         "stat",
			Path:       norm,
			StatusCode: http.StatusNotFound,
			Message:    "volume path not found",
			Err:        ErrVolumePathNotFound,
		}
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// Exists reports whether a file or directory exists at path.
func (vc *VolumeContent) Exists(ctx context.Context, targetPath string) (bool, error) {
	_, err := vc.Stat(ctx, targetPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrVolumePathNotFound) {
		return false, nil
	}
	return false, err
}

// Read downloads a file as a UTF-8 string.
func (vc *VolumeContent) Read(ctx context.Context, targetPath string) (string, error) {
	data, err := vc.ReadBytes(ctx, targetPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReadBytes downloads a file as raw bytes.
func (vc *VolumeContent) ReadBytes(ctx context.Context, targetPath string) ([]byte, error) {
	rc, err := vc.ReadStream(ctx, targetPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ReadStream opens a streaming reader for the file at path. The caller must
// close the returned io.ReadCloser.
func (vc *VolumeContent) ReadStream(ctx context.Context, targetPath string) (io.ReadCloser, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	if norm == "/" {
		return nil, fmt.Errorf("the volume root is a directory and cannot be read as a file")
	}
	var rc io.ReadCloser
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		key, keyErr := buildObjectKey(sess.creds.Prefix, norm, false)
		if keyErr != nil {
			return keyErr
		}
		stream, getErr := sess.storage.GetObject(ctx, key)
		if getErr != nil {
			return getErr
		}
		rc = stream
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// Write uploads data to path directly to object storage.
func (vc *VolumeContent) Write(ctx context.Context, targetPath string, data []byte, opts VolumeWriteOptions) (*VolumeEntry, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	if norm == "/" {
		return nil, fmt.Errorf("the volume root cannot be written as a file")
	}
	meta, err := validateAndBuildMetadata(opts.UID, opts.GID, opts.Mode)
	if err != nil {
		return nil, err
	}
	putOpts := s3PutOptions{Metadata: meta}
	if opts.Force != nil && !*opts.Force {
		putOpts.IfNoneMatch = "*"
	}
	var entry VolumeEntry
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		key, keyErr := buildObjectKey(sess.creds.Prefix, norm, false)
		if keyErr != nil {
			return keyErr
		}
		if putErr := sess.storage.PutObject(ctx, key, data, putOpts); putErr != nil {
			return putErr
		}
		entry = makeVolumeEntry(norm, VolumeFileTypeFile, int64(len(data)), vc.clock().UTC(), meta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// WriteFrom streams src to path without buffering the entire file in memory.
// The caller owns src; WriteFrom never closes it. Because an arbitrary
// io.Reader cannot be replayed, WriteFrom does not retry after streaming starts.
func (vc *VolumeContent) WriteFrom(ctx context.Context, targetPath string, src io.Reader, opts VolumeWriteOptions) (*VolumeEntry, error) {
	if src == nil {
		return nil, fmt.Errorf("source reader must not be nil")
	}
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	if norm == "/" {
		return nil, fmt.Errorf("the volume root cannot be written as a file")
	}
	meta, err := validateAndBuildMetadata(opts.UID, opts.GID, opts.Mode)
	if err != nil {
		return nil, err
	}
	sess, err := vc.ensureSession(ctx, false)
	if err != nil {
		return nil, err
	}
	key, err := buildObjectKey(sess.creds.Prefix, norm, false)
	if err != nil {
		return nil, err
	}
	putOpts := s3PutOptions{Metadata: meta}
	if opts.Force != nil && !*opts.Force {
		putOpts.IfNoneMatch = "*"
	}
	size, err := sess.storage.PutObjectStream(ctx, key, src, putOpts)
	if err != nil {
		if errors.Is(err, ErrVolumeCredentialsExpired) {
			vc.invalidateSession(sess)
		}
		return nil, err
	}
	entry := makeVolumeEntry(norm, VolumeFileTypeFile, size, vc.clock().UTC(), meta)
	return &entry, nil
}

// MakeDir creates a directory marker at path.
func (vc *VolumeContent) MakeDir(ctx context.Context, targetPath string, opts VolumeMakeDirOptions) (*VolumeEntry, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	meta, err := validateAndBuildMetadata(opts.UID, opts.GID, opts.Mode)
	if err != nil {
		return nil, err
	}
	noOverwrite := opts.Force != nil && !*opts.Force
	var entry VolumeEntry
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		if norm == "/" {
			if noOverwrite {
				return &VolumeStorageError{
					Op:         "mkdir",
					Path:       norm,
					StatusCode: http.StatusPreconditionFailed,
					Message:    "directory already exists",
					Err:        ErrVolumePathExists,
				}
			}
			entry = makeVolumeEntry("/", VolumeFileTypeDirectory, 0, vc.clock().UTC(), meta)
			return nil
		}

		// Refuse creating a directory over an existing file.
		fileKey, err := buildObjectKey(sess.creds.Prefix, norm, false)
		if err != nil {
			return err
		}
		if _, headErr := sess.storage.HeadObject(ctx, fileKey); headErr == nil {
			return &VolumeStorageError{
				Op:         "mkdir",
				Path:       norm,
				StatusCode: http.StatusConflict,
				Message:    "a file already exists at path",
				Err:        ErrVolumePathExists,
			}
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		dirKey, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		if noOverwrite {
			page, listErr := sess.storage.ListObjectsV2(ctx, dirKey, "", "", 1)
			if listErr != nil {
				return listErr
			}
			if page.KeyCount > 0 || len(page.Objects) > 0 || len(page.CommonPrefixes) > 0 {
				return &VolumeStorageError{
					Op:         "mkdir",
					Path:       norm,
					StatusCode: http.StatusPreconditionFailed,
					Message:    "directory already exists",
					Err:        ErrVolumePathExists,
				}
			}
		}

		putOpts := s3PutOptions{Metadata: meta}
		if noOverwrite {
			putOpts.IfNoneMatch = "*"
		}
		if putErr := sess.storage.PutObject(ctx, dirKey, []byte{}, putOpts); putErr != nil {
			return putErr
		}
		entry = makeVolumeEntry(norm, VolumeFileTypeDirectory, 0, vc.clock().UTC(), meta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// UpdateMetadata updates stored POSIX-like metadata (uid, gid, mode) on an
// existing file or directory via S3 CopyObject metadata replacement.
func (vc *VolumeContent) UpdateMetadata(ctx context.Context, targetPath string, opts VolumeUpdateMetadataOptions) (*VolumeEntry, error) {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return nil, err
	}
	if norm == "/" {
		return nil, fmt.Errorf("cannot update metadata on the volume root")
	}
	updates, err := validateAndBuildMetadata(opts.UID, opts.GID, opts.Mode)
	if err != nil {
		return nil, err
	}
	if len(updates) == 0 {
		return nil, fmt.Errorf("at least one of UID, GID, or Mode must be specified")
	}

	var entry VolumeEntry
	err = vc.withSession(ctx, func(sess *volumeSession) error {
		fileKey, err := buildObjectKey(sess.creds.Prefix, norm, false)
		if err != nil {
			return err
		}
		if head, headErr := sess.storage.HeadObject(ctx, fileKey); headErr == nil {
			merged := mergeMetadata(head.Metadata, updates)
			if copyErr := sess.storage.CopyObject(ctx, fileKey, fileKey, s3CopyOptions{
				Metadata:    merged,
				ContentType: head.ContentType,
			}); copyErr != nil {
				return copyErr
			}
			entry = makeVolumeEntry(norm, VolumeFileTypeFile, head.Size, vc.clock().UTC(), merged)
			return nil
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		dirKey, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		if head, headErr := sess.storage.HeadObject(ctx, dirKey); headErr == nil {
			merged := mergeMetadata(head.Metadata, updates)
			if copyErr := sess.storage.CopyObject(ctx, dirKey, dirKey, s3CopyOptions{
				Metadata:    merged,
				ContentType: head.ContentType,
			}); copyErr != nil {
				return copyErr
			}
			entry = makeVolumeEntry(norm, VolumeFileTypeDirectory, 0, vc.clock().UTC(), merged)
			return nil
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		// Implicit directory with existing children: materialize a directory
		// marker carrying the requested metadata.
		page, listErr := sess.storage.ListObjectsV2(ctx, dirKey, "", "", 1)
		if listErr != nil {
			return listErr
		}
		if page.KeyCount > 0 || len(page.Objects) > 0 || len(page.CommonPrefixes) > 0 {
			if putErr := sess.storage.PutObject(ctx, dirKey, []byte{}, s3PutOptions{Metadata: updates}); putErr != nil {
				return putErr
			}
			entry = makeVolumeEntry(norm, VolumeFileTypeDirectory, 0, vc.clock().UTC(), updates)
			return nil
		}

		return &VolumeStorageError{
			Op:         "update-metadata",
			Path:       norm,
			StatusCode: http.StatusNotFound,
			Message:    "volume path not found",
			Err:        ErrVolumePathNotFound,
		}
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// Remove deletes a single file or an empty directory at path.
//
// Unlike raw S3 DeleteObject, Remove never reports silent success for missing
// paths or non-empty directories:
//   - missing path -> ErrVolumePathNotFound
//   - non-empty directory -> ErrVolumeDirNotEmpty
//   - volume root "/" -> validation error
func (vc *VolumeContent) Remove(ctx context.Context, targetPath string) error {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("removing the volume root is not allowed")
	}
	return vc.withSession(ctx, func(sess *volumeSession) error {
		fileKey, err := buildObjectKey(sess.creds.Prefix, norm, false)
		if err != nil {
			return err
		}
		if _, headErr := sess.storage.HeadObject(ctx, fileKey); headErr == nil {
			return sess.storage.DeleteObject(ctx, fileKey)
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		dirKey, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		page, listErr := sess.storage.ListObjectsV2(ctx, dirKey, "", "", 2)
		if listErr != nil {
			return listErr
		}
		if len(page.Objects) == 0 {
			return &VolumeStorageError{
				Op:         "remove",
				Path:       norm,
				StatusCode: http.StatusNotFound,
				Message:    "volume path not found",
				Err:        ErrVolumePathNotFound,
			}
		}
		for _, obj := range page.Objects {
			if obj.Key != dirKey {
				return &VolumeStorageError{
					Op:         "remove",
					Path:       norm,
					StatusCode: http.StatusConflict,
					Message:    "directory is not empty",
					Err:        ErrVolumeDirNotEmpty,
				}
			}
		}
		return sess.storage.DeleteObject(ctx, dirKey)
	})
}

// RemoveAll deletes a file or recursively removes a directory and all of its
// descendants. Removing the volume root "/" is disallowed. Missing paths return
// ErrVolumePathNotFound.
func (vc *VolumeContent) RemoveAll(ctx context.Context, targetPath string) error {
	norm, err := normalizeVolumePath(targetPath)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("removing the volume root is not allowed")
	}
	return vc.withSession(ctx, func(sess *volumeSession) error {
		removedAny := false
		fileKey, err := buildObjectKey(sess.creds.Prefix, norm, false)
		if err != nil {
			return err
		}
		if _, headErr := sess.storage.HeadObject(ctx, fileKey); headErr == nil {
			if delErr := sess.storage.DeleteObject(ctx, fileKey); delErr != nil {
				return delErr
			}
			removedAny = true
		} else if !errors.Is(headErr, ErrVolumePathNotFound) {
			return headErr
		}

		dirKey, err := buildObjectKey(sess.creds.Prefix, norm, true)
		if err != nil {
			return err
		}
		token := ""
		for {
			page, listErr := sess.storage.ListObjectsV2(ctx, dirKey, "", token, 0)
			if listErr != nil {
				return listErr
			}
			for _, obj := range page.Objects {
				if delErr := sess.storage.DeleteObject(ctx, obj.Key); delErr != nil {
					return delErr
				}
				removedAny = true
			}
			if !page.IsTruncated {
				break
			}
			if page.NextContinuationToken == "" {
				return &VolumeStorageError{Op: "remove-all", Path: norm, Message: "truncated list page missing continuation token"}
			}
			token = page.NextContinuationToken
		}
		if !removedAny {
			return &VolumeStorageError{
				Op:         "remove-all",
				Path:       norm,
				StatusCode: http.StatusNotFound,
				Message:    "volume path not found",
				Err:        ErrVolumePathNotFound,
			}
		}
		return nil
	})
}

func validateVolumeContentCredentials(creds *VolumeContentCredentials, now time.Time, policy s3StoragePolicy) error {
	if creds == nil {
		return fmt.Errorf("volume content credentials must not be nil")
	}
	if creds.Provider != "s3" && creds.Provider != "cos" {
		return fmt.Errorf("unsupported direct-storage provider %q (expected 's3' or 'cos')", creds.Provider)
	}
	if err := validateBucketName(creds.Bucket); err != nil {
		return err
	}
	if strings.TrimSpace(creds.Region) == "" || hasControlOrSpace(creds.Region) {
		return fmt.Errorf("invalid storage region")
	}
	if err := validateVolumePrefix(creds.Prefix); err != nil {
		return err
	}
	if creds.AddressingStyle == "" {
		creds.AddressingStyle = "auto"
	}
	if creds.AddressingStyle != "auto" && creds.AddressingStyle != "path" && creds.AddressingStyle != "virtual" {
		return fmt.Errorf("addressing_style must be 'auto', 'path', or 'virtual', got %q", creds.AddressingStyle)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return fmt.Errorf("storage credentials must include access_key_id and secret_access_key")
	}
	if creds.ExpiresAt <= 0 {
		return fmt.Errorf("storage credentials must include a positive expires_at timestamp")
	}
	expiresAt := time.Unix(creds.ExpiresAt, 0).UTC()
	if !expiresAt.After(now) {
		return fmt.Errorf("%w: expires_at is not in the future", ErrVolumeCredentialsExpired)
	}
	parsed, err := validateStorageEndpoint(creds.Endpoint, policy)
	if err != nil {
		return err
	}
	if _, err := resolveS3AddressingStyle(creds.AddressingStyle, creds.Bucket, parsed.Hostname()); err != nil {
		return err
	}
	return nil
}

func validateBucketName(bucket string) error {
	if bucket == "" || len(bucket) > 255 {
		return fmt.Errorf("invalid storage bucket name")
	}
	if strings.ContainsAny(bucket, "/\\ \t\r\n\x00") || strings.Contains(bucket, "..") || hasControlOrSpace(bucket) {
		return fmt.Errorf("invalid storage bucket name")
	}
	return nil
}

// validateVolumePrefix accepts an empty prefix or a relative key prefix ending
// in "/" without leading "/", empty segments, ".", "..", backslashes, or
// control characters. Prefix is chosen by the plugin and is not pinned to the
// Volume ID.
func validateVolumePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("storage prefix must not start with '/'")
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("storage prefix must be empty or end with '/'")
	}
	if strings.ContainsAny(prefix, "\\\x00") || hasControlRune(prefix) {
		return fmt.Errorf("storage prefix contains disallowed characters")
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("storage prefix must not contain empty, '.', or '..' segments")
		}
	}
	return nil
}

// normalizeVolumePath validates that raw is an absolute POSIX path without NUL,
// backslash, control characters, or "."/".." segments, and collapses repeated
// slashes cleanly.
func normalizeVolumePath(raw string) (string, error) {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("volume path must be an absolute POSIX path, got %q", raw)
	}
	if strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\\') || hasControlRune(raw) {
		return "", fmt.Errorf("volume path must not contain NUL, backslash, or control characters")
	}
	rawSegs := strings.Split(raw, "/")
	var clean []string
	for _, seg := range rawSegs {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("volume path must not contain '.' or '..' segments, got %q", raw)
		}
		clean = append(clean, seg)
	}
	if len(clean) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(clean, "/"), nil
}

func buildObjectKey(prefix, normPath string, directory bool) (string, error) {
	if err := validateVolumePrefix(prefix); err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(normPath, "/")
	key := prefix + rel
	if directory && key != "" && !strings.HasSuffix(key, "/") {
		key += "/"
	}
	if !strings.HasPrefix(key, prefix) {
		return "", fmt.Errorf("resolved object key escapes volume prefix scope")
	}
	return key, nil
}

func objectKeyToVolumePath(prefix, key string) (string, error) {
	if !strings.HasPrefix(key, prefix) {
		return "", fmt.Errorf("object key outside prefix scope")
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "/")
	if rel == "" {
		return "/", nil
	}
	return normalizeVolumePath("/" + rel)
}

func joinVolumePath(basePath, rel string) string {
	if basePath == "/" {
		return "/" + rel
	}
	return basePath + "/" + rel
}

func validateAndBuildMetadata(uid, gid, mode *int) (map[string]string, error) {
	meta := make(map[string]string)
	if uid != nil {
		if *uid < 0 {
			return nil, fmt.Errorf("uid must be a non-negative integer, got %d", *uid)
		}
		meta["uid"] = strconv.Itoa(*uid)
	}
	if gid != nil {
		if *gid < 0 {
			return nil, fmt.Errorf("gid must be a non-negative integer, got %d", *gid)
		}
		meta["gid"] = strconv.Itoa(*gid)
	}
	if mode != nil {
		if *mode < 0 {
			return nil, fmt.Errorf("mode must be a non-negative integer, got %d", *mode)
		}
		meta["mode"] = strconv.FormatInt(int64(*mode), 8)
	}
	return meta, nil
}

func mergeMetadata(existing, updates map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(updates))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}
	return merged
}

func makeVolumeEntry(entryPath string, kind VolumeFileType, size int64, stamp time.Time, metadata map[string]string) VolumeEntry {
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	} else {
		stamp = stamp.UTC()
	}
	mode := defaultFileMode
	if kind == VolumeFileTypeDirectory {
		mode = defaultDirMode
	}
	uid, gid := 0, 0
	if metadata != nil {
		if rawMode, ok := metadata["mode"]; ok && rawMode != "" {
			if parsed, err := strconv.ParseInt(rawMode, 8, 32); err == nil && parsed >= 0 {
				mode = int(parsed)
			}
		}
		if rawUID, ok := metadata["uid"]; ok && rawUID != "" {
			if parsed, err := strconv.Atoi(rawUID); err == nil && parsed >= 0 {
				uid = parsed
			}
		}
		if rawGID, ok := metadata["gid"]; ok && rawGID != "" {
			if parsed, err := strconv.Atoi(rawGID); err == nil && parsed >= 0 {
				gid = parsed
			}
		}
	}
	name := "/"
	if entryPath != "/" {
		name = path.Base(strings.TrimSuffix(entryPath, "/"))
	}
	return VolumeEntry{
		Name:  name,
		Type:  kind,
		Path:  entryPath,
		Size:  size,
		Mode:  mode,
		UID:   uid,
		GID:   gid,
		Atime: stamp,
		Mtime: stamp,
		Ctime: stamp,
	}
}

func sortedVolumeEntries(byPath map[string]VolumeEntry) []VolumeEntry {
	entries := make([]VolumeEntry, 0, len(byPath))
	for _, entry := range byPath {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries
}

func hasControlOrSpace(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
