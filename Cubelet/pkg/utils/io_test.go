// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"os"
	"path"
	"testing"
)

func TestCopyFile(t *testing.T) {
	tempDir := t.TempDir()

	srcPath := path.Join(tempDir, "test_src.txt")
	dstPath := path.Join(tempDir, "test_dst.txt")
	srcContent := "Hello, world!"
	err := os.WriteFile(srcPath, []byte(srcContent), 0666)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(srcPath)
	defer os.Remove(dstPath)

	err = SafeCopyFile(dstPath, srcPath)
	if err != nil {
		t.Fatal(err)
	}

	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(dstContent) != srcContent {
		t.Fatalf("unexpected content: %s", string(dstContent))
	}
}
