// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pmem

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
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

func TestExplicitEvictionWaitsForGCBorrower(t *testing.T) {
	release, ok := TryEvictArtifact("cubebox", "concurrent-eviction")
	if !ok {
		t.Fatal("could not acquire GC lease")
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := EvictArtifact(ctx, "cubebox", "concurrent-eviction"); err != context.Canceled {
		t.Fatalf("cancelled wait: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		releaseNext, ok, err := EvictArtifact(context.Background(), "cubebox", "concurrent-eviction")
		if err == nil && !ok {
			err = fmt.Errorf("GC lease reported as a sandbox borrower")
		}
		if releaseNext != nil {
			releaseNext()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("eviction did not wait: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("eviction did not resume")
	}
}
