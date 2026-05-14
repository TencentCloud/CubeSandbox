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

var compareKernelFiles = sameFileSHA256

// EnsureKernelFilePresent verifies that the target kernel file is already present and valid.
func EnsureKernelFilePresent(ctx context.Context, sharedKernelPath, targetKernelPath string) error {
	if err := requireValidSharedKernel(sharedKernelPath); err != nil {
		return err
	}

	targetState, err := inspectKernelFileState(targetKernelPath)
	if err != nil {
		return err
	}
	switch targetState {
	case kernelFileStateValid:
		return nil
	case kernelFileStateMissing:
		return fmt.Errorf("target kernel file %s not exist", targetKernelPath)
	case kernelFileStateInvalid:
		return fmt.Errorf("target kernel file %s is invalid", targetKernelPath)
	default:
		return fmt.Errorf("unknown kernel file state for %s", targetKernelPath)
	}
}

// RefreshKernelFile rewrites the target from the current shared kernel.
func RefreshKernelFile(ctx context.Context, sharedKernelPath, targetKernelPath string) error {
	if err := requireValidSharedKernel(sharedKernelPath); err != nil {
		return err
	}
	if err := copyKernelFileAtomically(sharedKernelPath, targetKernelPath); err != nil {
		return err
	}
	if err := validateRefreshedKernelFile(sharedKernelPath, targetKernelPath); err != nil {
		cleanupErr := cleanupKernelFile(targetKernelPath)
		if cleanupErr != nil {
			log.G(ctx).Errorf(
				"kernel file %s verification failed against shared kernel %s and cleanup failed: verifyErr=%v cleanupErr=%v",
				targetKernelPath, sharedKernelPath, err, cleanupErr,
			)
			return fmt.Errorf("%w: cleanup invalid kernel file failed: %v", err, cleanupErr)
		}
		log.G(ctx).Errorf(
			"kernel file %s verification failed against shared kernel %s, cleaned up invalid target: %v",
			targetKernelPath, sharedKernelPath, err,
		)
		return err
	}
	log.G(ctx).Infof("kernel file %s refreshed from latest shared kernel %s", targetKernelPath, sharedKernelPath)
	return nil
}

type kernelFileState string

const (
	kernelFileStateValid   kernelFileState = "valid"
	kernelFileStateMissing kernelFileState = "missing"
	kernelFileStateInvalid kernelFileState = "invalid"
)

func requireValidSharedKernel(sharedKernelPath string) error {
	sharedState, err := inspectKernelFileState(sharedKernelPath)
	if err != nil {
		return err
	}
	switch sharedState {
	case kernelFileStateValid:
		return nil
	case kernelFileStateMissing:
		return fmt.Errorf("local shared kernel not found: %s", sharedKernelPath)
	default:
		return fmt.Errorf("local shared kernel validation failed: %w", validateKernelFile(sharedKernelPath, "local shared"))
	}
}

func inspectKernelFileState(path string) (kernelFileState, error) {
	exist, err := utils.FileExistAndValid(path)
	if err != nil {
		return kernelFileStateInvalid, nil
	}
	if exist {
		return kernelFileStateValid, nil
	}
	return kernelFileStateMissing, nil
}

func validateRefreshedKernelFile(sharedKernelPath, targetKernelPath string) error {
	if err := validateKernelFile(targetKernelPath, "refreshed"); err != nil {
		return err
	}
	same, err := compareKernelFiles(sharedKernelPath, targetKernelPath)
	if err != nil {
		return fmt.Errorf("refreshed kernel file %s verification failed against shared kernel %s: %w", targetKernelPath, sharedKernelPath, err)
	}
	if !same {
		return fmt.Errorf("refreshed kernel file %s differs from shared kernel %s", targetKernelPath, sharedKernelPath)
	}
	return nil
}

func validateKernelFile(path, operation string) error {
	exist, err := utils.FileExistAndValid(path)
	if err != nil {
		return fmt.Errorf("%s kernel file %s validation failed: %v", operation, path, err)
	}
	if !exist {
		return fmt.Errorf("%s kernel file %s not exist", operation, path)
	}
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

func cleanupKernelFile(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
