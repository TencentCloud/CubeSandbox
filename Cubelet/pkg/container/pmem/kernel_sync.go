// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// SyncKernelFile keeps targetKernelPath aligned with the current shared kernel.
func SyncKernelFile(ctx context.Context, sharedKernelPath, targetKernelPath string) error {
	sharedExist, err := utils.FileExistAndValid(sharedKernelPath)
	if err != nil {
		return fmt.Errorf("local shared kernel validation failed: %w", err)
	}
	if !sharedExist {
		return fmt.Errorf("local shared kernel not found: %s", sharedKernelPath)
	}

	targetExist, err := utils.FileExistAndValid(targetKernelPath)
	if err != nil {
		log.G(ctx).Warnf("kernel file %s validation failed, refresh from shared kernel: %v", targetKernelPath, err)
	}
	if !targetExist {
		if err := copyKernelFileAtomically(sharedKernelPath, targetKernelPath); err != nil {
			return err
		}
		targetExist, err = utils.FileExistAndValid(targetKernelPath)
		if err != nil {
			return fmt.Errorf("copied kernel file %s validation failed: %v", targetKernelPath, err)
		}
		if !targetExist {
			return fmt.Errorf("copied kernel file %s not exist", targetKernelPath)
		}
		log.G(ctx).Infof("kernel file %s missing, copied latest shared kernel from %s", targetKernelPath, sharedKernelPath)
		return nil
	}

	same, err := sameFileSHA256(sharedKernelPath, targetKernelPath)
	if err != nil {
		return err
	}
	if same {
		log.G(ctx).Infof("kernel file %s already matches latest shared kernel %s", targetKernelPath, sharedKernelPath)
		return nil
	}

	if err := copyKernelFileAtomically(sharedKernelPath, targetKernelPath); err != nil {
		return err
	}
	same, err = sameFileSHA256(sharedKernelPath, targetKernelPath)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("refreshed kernel file %s still differs from shared kernel %s", targetKernelPath, sharedKernelPath)
	}
	log.G(ctx).Infof("kernel file %s refreshed from latest shared kernel %s", targetKernelPath, sharedKernelPath)
	return nil
}

func sameFileSHA256(pathA, pathB string) (bool, error) {
	shaA, err := fileSHA256(pathA)
	if err != nil {
		return false, err
	}
	shaB, err := fileSHA256(pathB)
	if err != nil {
		return false, err
	}
	return shaA == shaB, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
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

func copyKernelFileAtomically(srcPath, dstPath string) error {
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
	return nil
}
