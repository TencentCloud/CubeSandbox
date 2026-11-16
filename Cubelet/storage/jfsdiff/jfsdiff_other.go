// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package jfsdiff

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("jfsdiff requires linux (FIEMAP, SEEK_DATA, fallocate)")

func dataMap(string) ([]mapping, int64, error)                    { return nil, 0, errUnsupported }
func dataMapFile(*os.File) ([]mapping, int64, error)              { return nil, 0, errUnsupported }
func punchHole(*os.File, int64, int64) error                      { return errUnsupported }
func nextData(*os.File, int64, int64) (int64, int64, bool, error) { return 0, 0, false, errUnsupported }
func statFile(string) (uint64, int64, int64, error)               { return 0, 0, 0, errUnsupported }
