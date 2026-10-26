// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sysctl

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

const (
	sysctlDir = "/proc/sys/"
)

var ErrInvalidKey = errors.New("could not find the given key")

func Get(name string) (string, error) {
	if runtime.GOOS != "linux" {
		os.Exit(1)
	}
	path := sysctlDir + strings.Replace(name, ".", "/", -1)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ErrInvalidKey
	}
	return strings.TrimSpace(string(data)), nil
}

func Set(name string, value string) error {
	if runtime.GOOS != "linux" {
		os.Exit(1)
	}
	path := sysctlDir + strings.Replace(name, ".", "/", -1)
	err := os.WriteFile(path, []byte(value), 0644)
	return err
}
