// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func CopyFile(ctx context.Context, src, dst string) error {

	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed to stat source path %s: %w", src, err)
	}

	if srcInfo.IsDir() {

		srcFS := os.DirFS(src)
		if err := os.CopyFS(dst, srcFS); err != nil {
			return fmt.Errorf("failed to copy directory from %s to %s: %w", src, dst, err)
		}
	} else {

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("failed to create parent directory for %s: %w", dst, err)
		}

		if err := copyFileFast(src, dst); err != nil {
			return fmt.Errorf("failed to copy file from %s to %s: %w", src, dst, err)
		}
	}

	log.G(ctx).WithFields(CubeLog.Fields{
		"copied_from": src,
		"copied_to":   dst,
	}).Debug("copy operation completed successfully")

	return nil
}

func copyFileFast(src, dst string) error {

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat source file: %w", err)
	}

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("failed to create dest file: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	return nil
}
