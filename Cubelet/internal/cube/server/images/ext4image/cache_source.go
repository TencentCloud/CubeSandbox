// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

const cacheMetadataName = ".rootfs-cache.json"

// CacheSource is runtime-only recovery metadata, never a template definition.
// Its token is stored only in an owner-readable file and never logged.
type CacheSource struct {
	ArtifactID   string `json:"artifact_id"`
	InstanceType string `json:"instance_type"`
	URL          string `json:"url"`
	Token        string `json:"token,omitempty"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	RemoteBacked bool   `json:"remote_backed"`
}

type cacheEntry struct {
	CacheSource
	LastUsed time.Time `json:"last_used"`
}

func (s *CacheSource) validate() error {
	if s.InstanceType != "cubebox" || pathutil.ValidateSafeID(s.ArtifactID) != nil {
		return errors.New("invalid cache artifact identity")
	}
	sha, err := hex.DecodeString(s.SHA256)
	if err != nil || len(sha) != 32 || s.SizeBytes <= 0 || !s.RemoteBacked {
		return errors.New("cache source must identify an object-store artifact with size and SHA256")
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid stable artifact gateway")
	}
	if u.Path != "/cube/template/artifact/download" || u.Query().Get("artifact_id") != s.ArtifactID {
		return errors.New("cache source must use the stable artifact download route")
	}
	q := u.Query()
	if s.Token == "" {
		s.Token = q.Get("token")
	}
	if s.Token == "" {
		return errors.New("cache source has no download credential")
	}
	q.Del("token")
	u.RawQuery, u.Fragment = q.Encode(), ""
	s.URL = u.String()
	return nil
}

func (s CacheSource) downloadURL() string {
	u, _ := url.Parse(rewriteDownloadHost(s.URL, cubeMasterHTTPAddr()))
	q := u.Query()
	q.Set("token", s.Token)
	u.RawQuery = q.Encode()
	return u.String()
}

func sourceFromSpec(instanceType, artifactID string, spec *cubeimages.ImageSpec) (*CacheSource, error) {
	if spec == nil || spec.Annotations[constants.MasterAnnotationRootfsArtifactCacheable] != "true" {
		return nil, nil
	}
	a := spec.Annotations
	size, _ := strconv.ParseInt(a[constants.MasterAnnotationRootfsArtifactSizeBytes], 10, 64)
	s := &CacheSource{ArtifactID: artifactID, InstanceType: instanceType,
		URL: a[constants.MasterAnnotationRootfsArtifactURL], Token: a[constants.MasterAnnotationRootfsArtifactToken],
		SHA256: a[constants.MasterAnnotationRootfsArtifactSHA256], SizeBytes: size, RemoteBacked: true}
	if u, err := url.Parse(s.URL); err == nil && isS3PresignedURL(u) {
		endpoint := cubeMasterHTTPAddr()
		if endpoint == "" {
			return nil, errors.New("a stable gateway is required before caching signed object-store downloads")
		}
		if !strings.Contains(endpoint, "://") {
			endpoint = "http://" + endpoint
		}
		s.URL = strings.TrimRight(endpoint, "/") + "/cube/template/artifact/download?artifact_id=" + url.QueryEscape(artifactID)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// cacheFile resolves only regular files under the configured managed root.
// A symlinked installation root is valid; per-artifact/file symlinks are not.
func cacheFile(artifactID string) (string, os.FileInfo, error) {
	if err := pathutil.ValidateSafeID(artifactID); err != nil {
		return "", nil, err
	}
	base, err := filepath.EvalSymlinks(pmem.GetPmemBasePath("cubebox"))
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(base, artifactID)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", nil, errors.New("cache artifact directory is missing or unsafe")
	}
	path := filepath.Join(dir, artifactID+".ext4")
	info, err = os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("cache rootfs is not a regular file")
	}
	return path, info, nil
}

func readCacheEntry(imagePath string) (*cacheEntry, error) {
	path := filepath.Join(filepath.Dir(imagePath), cacheMetadataName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("cache metadata is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entry cacheEntry
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&entry); err != nil {
		return nil, errors.New("invalid cache metadata")
	}
	if err := entry.CacheSource.validate(); err != nil {
		return nil, err
	}
	if filepath.Base(filepath.Dir(imagePath)) != entry.ArtifactID || entry.LastUsed.IsZero() {
		return nil, errors.New("cache metadata identity or access time is invalid")
	}
	return &entry, nil
}

func writeCacheEntry(imagePath string, entry *cacheEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(imagePath), ".rootfs-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(filepath.Dir(imagePath), cacheMetadataName))
}

func recordCacheUse(ctx context.Context, instanceType, artifactID string) error {
	if g := activeCacheGC.Load(); g == nil || !g.config.Enabled {
		return nil
	}
	if instanceType != "cubebox" {
		return nil
	}
	path, info, err := cacheFile(artifactID)
	if err != nil {
		return err
	}
	entry, readErr := readCacheEntry(path)
	source, err := sourceFromSpec(instanceType, artifactID, constants.GetImageSpec(ctx))
	if err != nil {
		return err
	}
	if source == nil && readErr != nil {
		return nil // Legacy/local-only files stay ineligible until explicitly registered.
	}
	if entry == nil {
		entry = &cacheEntry{}
	}
	if source != nil {
		entry.CacheSource = *source
	}
	if spec := constants.GetImageSpec(ctx); spec != nil && spec.Annotations[constants.MasterAnnotationRootfsArtifactCacheable] == "false" {
		return os.Remove(filepath.Join(filepath.Dir(path), cacheMetadataName))
	}
	if info.Size() != entry.SizeBytes {
		return errors.New("cache source size does not match the local file")
	}
	entry.LastUsed = time.Now()
	return writeCacheEntry(path, entry)
}

// ImportCacheSources adopts existing files without downloading/rebuilding
// templates. Initial access time is NOW, never the file's creation time.
func ImportCacheSources(ctx context.Context, manifest string) error {
	// ConfigMap projections use symlinks. Validate the opened target so a
	// symlink swap cannot change which file is checked versus decoded.
	f, err := os.OpenFile(manifest, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return errors.New("cache source manifest must be a bounded regular file")
	}
	var input struct {
		Version   int           `json:"version"`
		Artifacts []CacheSource `json:"artifacts"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&input); err != nil || input.Version != 1 {
		return errors.New("invalid rootfs cache source manifest")
	}
	for i := range input.Artifacts {
		if err := input.Artifacts[i].validate(); err != nil {
			return fmt.Errorf("invalid cache manifest entry %d: %w", i, err)
		}
	}
	for _, source := range input.Artifacts {
		release, err := pmem.PinArtifact(ctx, source.InstanceType, source.ArtifactID)
		if err != nil {
			return err
		}
		err = func() error {
			defer release()
			path, info, err := cacheFile(source.ArtifactID)
			if err != nil || info.Size() != source.SizeBytes {
				return nil // Not present, unsafe, or not the expected file: do not adopt.
			}
			unlock, err := pmemFileLocks.LockContext(ctx, pmem.GetRawImageFilePath(source.InstanceType, source.ArtifactID))
			if err != nil {
				return err
			}
			defer unlock()
			entry, err := readCacheEntry(path)
			if err != nil {
				entry = &cacheEntry{LastUsed: time.Now()}
			}
			entry.CacheSource = source
			return writeCacheEntry(path, entry)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
