// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func EnsurePmemFile(ctx context.Context, instanceType, imageRef string) error {
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
	if err := ensureKernelFile(ctx, instanceType, imageRef); err != nil {
		return err
	}
	return ensureImageVersionFile(ctx, instanceType, imageRef)
}

func tryDownloadPmemFile(ctx context.Context, imagePath string, spec *cubeimages.ImageSpec) error {
	if spec == nil || spec.Annotations == nil {
		return fmt.Errorf("image spec annotations are empty")
	}
	downloadURL := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactURL])
	if downloadURL == "" {
		return fmt.Errorf("artifact download url is empty")
	}
	downloadURL = rewriteDownloadHost(downloadURL)
	expectedSHA := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactSHA256])
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return err
	}
	tmpPath := imagePath + ".download"
	if err := os.RemoveAll(tmpPath); err != nil { // NOCC:Path Traversal()
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	if token := strings.TrimSpace(spec.Annotations[constants.MasterAnnotationRootfsArtifactToken]); token != "" {
		req.Header.Set("X-Cube-Artifact-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download status code %d", resp.StatusCode)
	}
	f, err := os.Create(tmpPath) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		return err
	}
	if expectedSHA != "" {
		gotSHA := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(gotSHA, expectedSHA) {
			return fmt.Errorf("artifact sha256 mismatch, got %s want %s", gotSHA, expectedSHA)
		}
	}
	if err := os.Rename(tmpPath, imagePath); err != nil {
		return err
	}
	return nil
}

func ensureKernelFile(ctx context.Context, instanceType, imageRef string) error {
	return pmem.SyncKernelFile(ctx, pmem.GetSharedKernelFilePath(), pmem.GetRawKernelFilePath(instanceType, imageRef))
}

func ensureImageVersionFile(ctx context.Context, instanceType, imageRef string) error {
	versionPath := pmem.GetRawImageVersionFilePath(instanceType, imageRef)
	exist, err := fileExistsAndNonEmpty(versionPath)
	if err != nil {
		log.G(ctx).Warnf("image version file %s validation failed, try copy: %v", versionPath, err)
	}
	if exist {
		return nil
	}
	sharedVersionPath := pmem.GetSharedImageVersionFilePath()
	sharedExist, err := fileExistsAndNonEmpty(sharedVersionPath)
	if err != nil {
		return fmt.Errorf("local shared image version validation failed: %w", err)
	}
	if !sharedExist {
		return fmt.Errorf("local shared image version not found: %s", sharedVersionPath)
	}
	if err := copyLocalFileAtomically(ctx, sharedVersionPath, versionPath, "image version"); err != nil {
		return err
	}
	exist, err = fileExistsAndNonEmpty(versionPath)
	if err != nil {
		return fmt.Errorf("copied image version file %s validation failed: %v", versionPath, err)
	}
	if !exist {
		return fmt.Errorf("copied image version file %s not exist", versionPath)
	}
	return nil
}

func copyLocalFileAtomically(ctx context.Context, srcPath, dstPath, fileType string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	tmpPath := dstPath + ".tmp"
	if err := os.RemoveAll(tmpPath); err != nil { // NOCC:Path Traversal()
		return err
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}
	dstFile, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode()) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		_ = os.RemoveAll(tmpPath) // NOCC:Path Traversal()
		return err
	}
	if err := dstFile.Close(); err != nil {
		_ = os.RemoveAll(tmpPath) // NOCC:Path Traversal()
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.RemoveAll(tmpPath) // NOCC:Path Traversal()
		return err
	}
	log.G(ctx).Infof("copied local %s from %s to %s", fileType, srcPath, dstPath)
	return nil
}

func fileExistsAndNonEmpty(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("%s is a directory", path)
	}
	return info.Size() > 0, nil
}

func rewriteDownloadHost(rawURL string) string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.MetaServerConfig == nil {
		return rawURL
	}
	endpoint := strings.TrimSpace(cfg.MetaServerConfig.MetaServerEndpoint)
	if endpoint == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Host = endpoint
	return u.String()
}
