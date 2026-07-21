// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/google/uuid"
)

const artifactPublishBufferSize = 4 * 1024 * 1024

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

// PublishExt4 copies a completed local ext4 image into an immutable shared
// generation path. The temporary file and final file live in the same CFS
// directory so rename is atomic for readers of that directory.
func PublishExt4(ctx context.Context, artifactID string, generation int64, shaValue string, srcPath string) (string, int64, error) {
	if artifactID == "" || generation < 1 || shaValue == "" || srcPath == "" {
		return "", 0, fmt.Errorf("invalid artifact publish arguments")
	}
	shaBytes, err := hex.DecodeString(shaValue)
	if err != nil || len(shaBytes) != sha256.Size || hex.EncodeToString(shaBytes) != shaValue {
		return "", 0, fmt.Errorf("invalid artifact SHA-256 %q", shaValue)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	storeDir, err := ResolveArtifactStoreDir(ctx, artifactID)
	if err != nil {
		return "", 0, err
	}
	generationDir := filepath.Join(storeDir, "generations", strconv.FormatInt(generation, 10))
	if err := os.MkdirAll(generationDir, 0o755); err != nil {
		return "", 0, err
	}
	finalPath := filepath.Join(generationDir, shaValue+".ext4")
	tmpPath := filepath.Join(generationDir, "."+shaValue+"."+uuid.NewString()+".partial")
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}
	cleanup := true
	renamed := false
	defer func() {
		_ = dst.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
			if renamed {
				_ = os.Remove(finalPath)
				if dir, err := os.Open(generationDir); err == nil {
					_ = dir.Sync()
					_ = dir.Close()
				}
			}
		}
	}()
	hasher := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(dst, hasher), contextReader{ctx: ctx, r: src}, make([]byte, artifactPublishBufferSize))
	if err != nil {
		return "", 0, err
	}
	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != shaValue {
		return "", 0, fmt.Errorf("artifact SHA-256 changed while publishing: got %s want %s", actualSHA, shaValue)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := dst.Sync(); err != nil {
		return "", 0, err
	}
	if err := dst.Close(); err != nil {
		return "", 0, err
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, err
	}
	renamed = true
	dir, err := os.Open(generationDir)
	if err != nil {
		return "", 0, err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return "", 0, err
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	cleanup = false
	return finalPath, written, nil
}

// RemovePublishedExt4 removes only the exact generation file returned by
// PublishExt4. It is used when the fenced database finalization fails after a
// successful copy, preventing an unreferenced multi-gigabyte artifact from
// being stranded in shared storage.
func RemovePublishedExt4(ctx context.Context, artifactID string, generation int64, publishedPath string) error {
	if artifactID == "" || generation < 1 || publishedPath == "" {
		return fmt.Errorf("invalid published artifact cleanup arguments")
	}
	storeDir, err := ResolveArtifactStoreDir(ctx, artifactID)
	if err != nil {
		return err
	}
	generationDir := filepath.Join(storeDir, "generations", strconv.FormatInt(generation, 10))
	cleanPath := filepath.Clean(publishedPath)
	rel, err := filepath.Rel(generationDir, cleanPath)
	if err != nil || rel == "." || filepath.Dir(rel) != "." {
		return fmt.Errorf("published artifact path %q is outside generation directory %q", publishedPath, generationDir)
	}
	if err := os.Remove(cleanPath); err != nil && !os.IsNotExist(err) { // NOCC:Path Traversal()
		return err
	}
	if err := os.Remove(generationDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	parent, err := os.Open(filepath.Dir(generationDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}
