// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStdinCloserClosesOnceOnEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = reader.Close()
	})
	require.NoError(t, writer.Close())

	stdin := newStdinCloser(reader)
	var closeCount atomic.Int32
	stdin.SetCloser(func() {
		closeCount.Add(1)
	})

	n, err := stdin.Read(make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, int32(1), closeCount.Load())

	n, err = stdin.Read(make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, int32(1), closeCount.Load())
}
