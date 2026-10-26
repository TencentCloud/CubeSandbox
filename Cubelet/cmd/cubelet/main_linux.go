// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package main

import (
	"fmt"
	stdlog "log"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
)

func createNewCubeMnt() error {
	if err := bindNamespaceToPath(CubeMntNsDirPath); err != nil {
		stdlog.Fatalf("bind mnt namespace fail,%v", err)
	}
	stdlog.Default().Printf("create new mnt namespace at %s \n", CubeMntNsFilePath)
	return nil
}

func bindNamespaceToPath(baseDir string) error {
	var wg sync.WaitGroup
	wg.Add(1)

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return err
	}

	targetPath := CubeMntNsFilePath

	mountPointFd, err := os.OpenFile(targetPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating target file: %v\n", err)
		os.Exit(1)
	}
	defer mountPointFd.Close()
	defer func() {

		if err != nil {
			os.RemoveAll(targetPath)
		}
	}()

	go (func() {
		defer wg.Done()
		runtime.LockOSThread()

		err = unix.Unshare(unix.CLONE_NEWNET)
		if err != nil {
			return
		}

		err = unix.Mount(getCurrentThreadNetNSPath(), targetPath, "none", unix.MS_BIND, "")
		if err != nil {
			err = fmt.Errorf("failed to bind mount ns at %s: %w", targetPath, err)
		}
	})()
	wg.Wait()

	return nil
}

func getCurrentThreadNetNSPath() string {

	return fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid())
}

func getMntNSPathFromPID(pid uint32) string {
	return fmt.Sprintf("/proc/%d/ns/mnt", pid)
}

func setMntNs(mntPath string) error {
	targetNs, err := unix.Open(mntPath, unix.O_RDONLY, 0)
	if err != nil {
		stdlog.Fatalf("open mnt file fail,%v", err)
	}

	if err := unix.Setns(targetNs, unix.CLONE_NEWNS); err != nil {
		stdlog.Fatalf("setns fail,%v", err)
	}
	stdlog.Default().Printf("nsenter to %s \n", mntPath)
	return nil

}
