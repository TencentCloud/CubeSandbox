// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

type gcRecordingCubeVSAdapter struct {
	fakeCubeVSAdapter
	keep         map[uint32]struct{}
	stillPresent func(uint32) bool
	onConflict   func(uint32)
}

func (f *gcRecordingCubeVSAdapter) GCStaleNetPolicyMaps(keep map[uint32]struct{}, stillPresent func(uint32) bool, onConflict func(uint32)) (int, error) {
	f.keep = keep
	f.stillPresent = stillPresent
	f.onConflict = onConflict
	return len(keep), nil
}

func TestRunStaleNetPolicyMapGCKeepsLiveAndPoolIfindices(t *testing.T) {
	adapter := &gcRecordingCubeVSAdapter{}
	pool, err := NewTapPool()
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &NetworkController{
		cubevsAdapter: adapter,
		tapAdapter: &fakeTapDeviceAdapter{listResult: map[string]*tapDevice{
			"z192.168.0.1": {Name: "z192.168.0.1", Index: 101, IP: net.ParseIP("192.168.0.1")},
			"z192.168.0.2": {Name: "z192.168.0.2", Index: 102, IP: net.ParseIP("192.168.0.2")},
		}},
		tapPool: pool,
	}
	entry, err := NewReadyTapPoolEntry("z192.168.0.3", 103, net.ParseIP("192.168.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ctrl.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}

	ctrl.runStaleNetPolicyMapGC()

	for _, want := range []uint32{101, 102, 103} {
		if _, ok := adapter.keep[want]; !ok {
			t.Fatalf("keep missing ifindex %d: %#v", want, adapter.keep)
		}
	}
	if adapter.stillPresent == nil {
		t.Fatal("stillPresent callback was not passed to GC")
	}
	if adapter.onConflict == nil {
		t.Fatal("onConflict callback was not passed to GC")
	}
}

func TestRunStaleNetPolicyMapGCSkipsWhenListTapsFails(t *testing.T) {
	adapter := &gcRecordingCubeVSAdapter{}
	ctrl := &NetworkController{
		cubevsAdapter: adapter,
		tapAdapter: &fakeTapDeviceAdapter{
			listErr: errors.New("dump interrupted"),
			listResult: map[string]*tapDevice{
				"z192.168.0.1": {Name: "z192.168.0.1", Index: 101, IP: net.ParseIP("192.168.0.1")},
			},
		},
	}

	ctrl.runStaleNetPolicyMapGC()

	if adapter.keep != nil {
		t.Fatalf("GC ran with incomplete keep %#v; List failure must skip GC", adapter.keep)
	}
}

func TestNetPolicyIfindexStillPresent(t *testing.T) {
	orig := netlinkLinkByIndex
	t.Cleanup(func() { netlinkLinkByIndex = orig })

	netlinkLinkByIndex = func(index int) (netlink.Link, error) {
		if index == 1 {
			return &netlink.Tuntap{}, nil
		}
		if index == 2 {
			return nil, netlink.LinkNotFoundError{}
		}
		return nil, errors.New("transient dump error")
	}

	if !netPolicyIfindexStillPresent(1) {
		t.Fatal("live link should be present")
	}
	if netPolicyIfindexStillPresent(2) {
		t.Fatal("LinkNotFound should mean absent")
	}
	if !netPolicyIfindexStillPresent(3) {
		t.Fatal("transient error should be treated as present")
	}
}

func TestRestoreDefaultDenyAfterStaleGCConflict(t *testing.T) {
	adapter := &fakeCubeVSAdapter{}
	ctrl := &NetworkController{cubevsAdapter: adapter}
	ctrl.restoreDefaultDenyAfterStaleGCConflict(42)
	if got := len(adapter.defaultDenyPolicyCalls); got != 1 || adapter.defaultDenyPolicyCalls[0] != 42 {
		t.Fatalf("defaultDenyPolicyCalls=%v, want [42]", adapter.defaultDenyPolicyCalls)
	}
}
