// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	mutex     sync.Mutex
	projectID = int(time.Now().Unix())
)

func DenExist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func FileExistAndValid(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return false, fmt.Errorf("%s is a directory", path)
		}
		if info.Size() > 1024 {
			return true, nil
		}
		return false, fmt.Errorf("invalid size:%d", info.Size())
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func FileExistWithSize(path string) (bool, int64, error) {
	info, err := os.Stat(path)
	if err == nil {
		return true, info.Size(), nil
	}
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	return false, 0, err
}

func GetDeviceIdleRatio(path string) (uint64, uint64, error) {
	var buf unix.Statfs_t
	err := unix.Statfs(path, &buf)
	if err != nil {
		return 0, 0, err
	}
	return uint64(100) * buf.Bavail / buf.Blocks, uint64(100) * buf.Ffree / buf.Files, nil
}

func GetAllDirname(pathname string) ([]string, error) {
	var s []string
	rd, err := os.ReadDir(pathname)
	if err != nil {
		return s, err
	}

	for _, fi := range rd {
		if fi.IsDir() {
			s = append(s, fi.Name())
		}
	}
	return s, nil
}

func IsEmpty(dirname string) (bool, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

//go:noinline
func LazyUnmount(dest string) error {
	return syscall.Unmount(dest, syscall.MNT_DETACH)
}

func WriteStrToFile(path, msg string) error {
	return os.WriteFile(path, []byte(msg), 0644)
}

func IsMountLoop(backendFile string) bool {
	subCmd := fmt.Sprintf("cat /proc/1/mountinfo | grep %s", backendFile)
	cmd := exec.CommandContext(context.Background(), "/usr/bin/bash", "-c", subCmd)
	_, err := cmd.Output()
	return err == nil
}

func GetLoopNameByBackendFile(file string) (string, error) {
	cmd := fmt.Sprintf("losetup -j %s -n -O NAME", file)
	var tm time.Duration = 3000
	stdout, _, err := Exec(cmd, tm)
	if err != nil {
		return "", err
	}
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return stdout, fmt.Errorf("NOT FOUND")
	}

	return stdout, nil
}

func GetMountNumByLoopName(loop string) (int, error) {

	cmd := fmt.Sprintf("cat /proc/mounts  | grep '%s ' | wc -l", loop)
	var tm time.Duration = 3000
	stdout, _, err := Exec(cmd, tm)
	if err != nil {
		return 0, err
	}
	stdout = strings.TrimSpace(stdout)
	num, err := strconv.Atoi(stdout)
	if err != nil {
		return 0, err
	}
	return num, nil
}

func GetMountNumByBackendFile(file string) (int, error) {
	stdout, err := GetLoopNameByBackendFile(file)
	if err != nil {
		return 0, err
	}

	return GetMountNumByLoopName(stdout)
}

func GetProjectID() int {
	mutex.Lock()
	defer mutex.Unlock()
	id := projectID
	projectID++
	return id
}

func QuotaDir(ctx context.Context, dir string, quota uint64, projectID int) error {
	defer Recover()

	xfsCmd := []string{"xfs_quota", "-x", "-c", fmt.Sprintf("project -s -p %s %d", dir, projectID)}
	if _, stderr, err := ExecV(xfsCmd, DefaultTimeout); err != nil {
		return fmt.Errorf("quota cmd[%s],failed.err:%s", xfsCmd, stderr)
	}

	xfsCmd = []string{"xfs_quota", "-x", "-c", fmt.Sprintf("limit -p bhard=%dm %d", quota, projectID)}
	if _, stderr, err := ExecV(xfsCmd, DefaultTimeout); err != nil {
		return fmt.Errorf("quota cmd[%s],failed.err:%s", xfsCmd, stderr)
	}

	return nil
}

func QuotaDirScale(ctx context.Context, limit uint64, projectID int) error {
	cmd := []string{"xfs_quota", "-x", "-c", fmt.Sprintf("limit -p bhard=%dm %d", limit, projectID)}
	_, _, err := ExecV(cmd, DefaultTimeout)
	if err != nil {
		return err
	}
	return nil
}

func MountSqfs(src, dst string, timeout time.Duration) error {
	cmd := []string{"mount", "-o", "ro", src, dst}
	_, stderr, err := ExecV(cmd, timeout)
	if err == nil {
		return nil
	}
	return fmt.Errorf("stderr:%s, err:%s", stderr, err)
}
