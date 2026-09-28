// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"errors"
	"testing"
)

func TestIsS3CloneSourceMustBeSnapshot(t *testing.T) {
	if isS3CloneSourceMustBeSnapshot(nil) {
		t.Fatal("nil must not match")
	}
	volErr := errors.New("invalid argument: 'sb-abc-memory' is a volume; create_volume_from_snapshot requires a snapshot as source")
	if !isS3CloneSourceMustBeSnapshot(volErr) {
		t.Fatalf("volume source should retry via snapshot, err=%v", volErr)
	}
	if isS3CloneSourceMustBeSnapshot(errors.New("not found: lvol not found")) {
		t.Fatal("unrelated errors must not look like a volume-source mismatch")
	}
}
