// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func TestNewExt4BaseRaw(t *testing.T) {
	testDir := t.TempDir()

	filePath := filepath.Join(testDir, "base.raw")
	if err := newExt4BaseRaw(filePath, defaultDiskUUID, 512000); err != nil {
		t.Fatal(err)
	}
}

func TestNewExt4RawByCopy(t *testing.T) {
	testDir := t.TempDir()

	fmt.Println(testDir)

	baseFile := filepath.Join(testDir, "base.raw")
	if err := newExt4BaseRaw(baseFile, defaultDiskUUID, 512000); err != nil {
		t.Fatal(err)
	}

	var err error

	targetFile := filepath.Join(testDir, "target.raw")
	err = newExt4RawByCopy(baseFile, targetFile, 0)
	assert.NoErrorf(t, err, "copy with size 0")

	targetFile = filepath.Join(testDir, "target2.raw")
	err = newExt4RawByCopy(baseFile, targetFile, 1024000)
	assert.NoErrorf(t, err, "copy with size 1024000")

	targetFile = filepath.Join(testDir, "target3.raw")
	err = newExt4RawByCopy(baseFile, targetFile, 128000)
	assert.NoErrorf(t, err, "copy with size 128000")
}

func TestNewExt4RawByReflinkCopy(t *testing.T) {
	utils.SkipCI(t)

	testDir := t.TempDir()

	fmt.Println(testDir)

	baseFile := filepath.Join(testDir, "base.raw")
	if err := newExt4BaseRaw(baseFile, defaultDiskUUID, 512000); err != nil {
		t.Fatal(err)
	}

	var err error

	targetFile := filepath.Join(testDir, "target.raw")
	err = newExt4RawByReflinkCopy(baseFile, targetFile, 0)
	if err != nil {

		t.Skipf("reflink copy not supported on this filesystem: %v", err)
		return
	}
	assert.NoErrorf(t, err, "copy with size 0")

	targetFile = filepath.Join(testDir, "target2.raw")
	err = newExt4RawByReflinkCopy(baseFile, targetFile, 1024000)
	assert.NoErrorf(t, err, "copy with size 1024000")

	targetFile = filepath.Join(testDir, "target3.raw")
	err = newExt4RawByReflinkCopy(baseFile, targetFile, 128000)
	assert.Errorf(t, err, "copy with size 128000")
}

func TestDescribeFile_Empty(t *testing.T) {
	assert.Equal(t, "<empty>", describeFile(""))
}

func TestDescribeFile_Missing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.raw")
	assert.Equal(t, "missing", describeFile(missing))
}

func TestDescribeFile_Size(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.raw")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "size=5", describeFile(f))
}

func TestDescribeFreeBytes_Invalid(t *testing.T) {
	got := describeFreeBytes(filepath.Join(t.TempDir(), "missing-dir"))
	assert.Truef(t, strings.HasPrefix(got, "statfs err="),
		"expected statfs err= prefix, got %q", got)
}

func TestDescribeFreeBytes_Valid(t *testing.T) {
	got := describeFreeBytes(t.TempDir())
	assert.Regexp(t, `^\d+B$`, got)
}

func TestDescribeStorageFailure_Format(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.raw")
	base := filepath.Join(dir, "base.raw")
	if err := os.WriteFile(target, []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("yyyyy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"cp", "--reflink=always", base, target},
		{"truncate", "-s", "1073741824", target},
		{"e2fsck", "-fy", target},
		{"resize2fs", target},
	}
	started := time.Now().Add(-500 * time.Millisecond)

	got := describeStorageFailure(cmds, 2, target, base, started)

	assert.Truef(t, strings.HasPrefix(got, " [step=3/4 "),
		"prefix: %q", got)
	assert.Contains(t, got, `cmd="e2fsck -fy `)
	assert.Contains(t, got, "target=size=2")
	assert.Contains(t, got, "base=size=5")
	assert.Contains(t, got, "elapsed=")
	assert.Contains(t, got, " free=")
	assert.Truef(t, strings.HasSuffix(got, "B]"),
		"suffix: %q", got)
}
