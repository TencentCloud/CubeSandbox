// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	dataDirName = "data"
	bindingFile = ".cube-volume-binding"
)

var bindingPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func requireDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("NFS volume path %q must be a real directory", dir)
	}
	return nil
}

func claimVolume(root, id string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("CUBE_NFS_CONTROL_ROOT must be a clean absolute path to the NFS export")
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("NFS control root is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("NFS control root must be a directory")
	}
	dir := filepath.Join(root, id)
	for _, target := range []string{dir, filepath.Join(dir, dataDirName)} {
		if err := requireDirectory(target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", &invalidArgumentError{message: "provision the NFS volume data directory before registering it with an explicit name", cause: err}
			}
			return "", err
		}
	}
	var nonce [16]byte
	// crypto/rand.Read cannot return an error on Go 1.24 and later.
	rand.Read(nonce[:])
	binding := hex.EncodeToString(nonce[:])
	file, err := os.CreateTemp(dir, ".binding-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(binding)
	if err == nil {
		err = file.Chmod(0644)
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	// Publish the complete binding without replacing anyone else's claim.
	if err := os.Link(file.Name(), filepath.Join(dir, bindingFile)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", &invalidArgumentError{message: "provision a fresh NFS volume directory instead of reusing an existing claim", cause: err}
		}
		return "", err
	}
	return binding, nil
}

func validateBinding(dir, expected string) error {
	marker := filepath.Join(dir, bindingFile)
	info, err := os.Lstat(marker)
	if err != nil {
		return fmt.Errorf("read NFS volume binding: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
		return errors.New("invalid NFS volume binding marker")
	}
	actual, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(actual) != expected {
		return errors.New("NFS volume directory belongs to a different registration")
	}
	return nil
}
