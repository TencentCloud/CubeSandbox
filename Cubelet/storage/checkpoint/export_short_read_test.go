// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadChunkRefusesAShortRead(t *testing.T) {
	src := bytes.NewReader([]byte("0123456789"))
	full := make([]byte, 4)
	if err := readChunk(src, full, 2); err != nil || string(full) != "2345" {
		t.Fatalf("full read: %q %v", full, err)
	}
	// A file that ends inside the chunk must fail, whatever the buffer held.
	stale := []byte("XXXXXX")
	err := readChunk(src, stale, 6)
	if err == nil || !strings.Contains(err.Error(), "short read: 4 of 6 bytes") {
		t.Fatalf("short read: %v", err)
	}
	if err := readChunk(src, make([]byte, 2), 10); err == nil {
		t.Fatal("a read past the end must fail")
	}
}
