// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteFile(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "test")
	f, err := os.OpenFile(testFile, os.O_RDWR|os.O_CREATE, 0755)
	require.NoErrorf(t, err, "open test file")
	defer f.Close()

	data := "m1234567890"

	_, err = f.Write([]byte(data))
	require.NoErrorf(t, err, "write test data")

	b := make([]byte, 1)
	n, _ := f.ReadAt(b, 0)
	if n == 1 {
		f.WriteAt(b, 0)
	}

	f.Seek(0, 0)
	out, err := io.ReadAll(f)
	assert.NoError(t, err)
	assert.Equal(t, data, string(out))
}
