// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pmem

import (
	"context"
	"errors"
	"testing"
)

func TestCacheLeasePinsAndEviction(t *testing.T) {
	first, err := PinArtifact(context.Background(), "cubebox", "lease-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PinArtifact(context.Background(), "cubebox", "lease-test")
	if err != nil {
		t.Fatal(err)
	}
	first()
	first() // Repeated release must not decrement another borrower's pin.
	if release, ok := TryEvictArtifact("cubebox", "lease-test"); ok {
		release()
		t.Fatal("evicted a pinned artifact")
	}
	second()
	evicted, ok := TryEvictArtifact("cubebox", "lease-test")
	if !ok {
		t.Fatal("idle artifact could not be locked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PinArtifact(ctx, "cubebox", "lease-test"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() { release, _ := PinArtifact(context.Background(), "cubebox", "lease-test"); acquired <- release }()
	evicted()
	(<-acquired)()
	if release, ok := TryEvictArtifact("cubebox", "lease-test"); !ok {
		t.Fatal("lease leaked")
	} else {
		release()
	}
}
