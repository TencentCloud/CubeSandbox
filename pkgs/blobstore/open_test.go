// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import (
	"context"
	"testing"
)

func TestOpenUnknownDriver(t *testing.T) {
	_, err := Open(context.Background(), Config{Driver: "nope"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRegisterNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Register(nil)
}
