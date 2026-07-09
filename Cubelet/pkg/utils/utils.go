// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func Int32ToBytes(n int32) []byte {
	bytebuf := bytes.NewBuffer([]byte{})
	binary.Write(bytebuf, binary.LittleEndian, n)
	return bytebuf.Bytes()
}

func Int64ToBytes(n int64) []byte {
	bytebuf := bytes.NewBuffer([]byte{})
	binary.Write(bytebuf, binary.LittleEndian, n)
	return bytebuf.Bytes()
}

func Min(arg int, args ...int) int {
	var m = arg
	for _, e := range args {
		if e < m {
			m = e
		}
	}
	return m
}

func GetDiskCap(path string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	if err != nil {
		return 0, 0, err
	}
	var blockRatio uint64
	if stat.Blocks > 0 {
		blockRatio = 100 * stat.Bavail / stat.Blocks
	}
	var inodeRatio uint64
	if stat.Files > 0 {
		inodeRatio = 100 * stat.Ffree / stat.Files
	} else {
		// Filesystems like btrfs don't maintain a fixed inode table
		// and report Files=0. Treat as 100% inode available.
		inodeRatio = 100
	}
	return blockRatio, inodeRatio, nil
}

func GetSha256Value(image string) (string, error) {
	imageUrl := strings.Split(image, "@")
	if len(imageUrl) != 2 {
		return "", fmt.Errorf("Image url should be xxx@sha256:xxxxx:%s", image)
	}
	return imageUrl[1], nil
}

func Contains(val string, slice []string) bool {
	return slices.Contains(slice, val)
}

func MapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func GetBodyData(rsp *http.Response, object any) error {
	if rsp.Body == nil {
		return fmt.Errorf("response body is nil")
	}
	data, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}
	err = JSONTool.Unmarshal(data, object)
	if err != nil {
		return err
	}
	return nil
}

func IsInteger(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
