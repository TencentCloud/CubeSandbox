// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"errors"
	"net"
	"testing"
)

func TestRecycleTapLockedStagesTapWithoutRestore(t *testing.T) {
	oldRestore := restoreTapFunc
	t.Cleanup(func() {
		restoreTapFunc = oldRestore
	})

	restoreTapFunc = func(*tapDevice, int, string, int) (*tapDevice, error) {
		t.Fatal("restoreTap should not be called when recycling tap to pool")
		return nil, nil
	}

	svc := newLifecycleTestService(t)
	tap := &tapDevice{
		Name:  "z192.168.0.2",
		Index: 12,
		IP:    net.ParseIP("192.168.0.2").To4(),
		File:  newTestTapFile(t),
		InUse: true,
	}

	svc.recycleTapLocked(tap)

	if len(svc.tapPool) != 1 {
		t.Fatalf("tapPool len=%d, want 1", len(svc.tapPool))
	}
	if svc.tapPool[0].File != nil {
		t.Fatal("pooled tap file should be closed and cleared")
	}
	if svc.tapPool[0].InUse {
		t.Fatal("pooled tap should not remain marked in use")
	}
}

func TestCreatePoolTapLockedStagesNewTapWithoutRestore(t *testing.T) {
	oldNewTap := newTapFunc
	oldRestore := restoreTapFunc
	oldPrepareTap := cubevsPrepareTAPPolicy
	t.Cleanup(func() {
		newTapFunc = oldNewTap
		restoreTapFunc = oldRestore
		cubevsPrepareTAPPolicy = oldPrepareTap
	})

	restoreCalls := 0
	restoreTapFunc = func(*tapDevice, int, string, int) (*tapDevice, error) {
		restoreCalls++
		return nil, nil
	}
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{
			Name:  tapName(ip.String()),
			Index: 21,
			IP:    ip,
			File:  newTestTapFile(t),
			InUse: true,
		}, nil
	}
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }

	svc := newLifecycleTestService(t)
	if err := svc.createPoolTap(); err != nil {
		t.Fatalf("createPoolTap error=%v", err)
	}

	if restoreCalls != 0 {
		t.Fatalf("restoreCalls=%d, want 0", restoreCalls)
	}
	if len(svc.tapPool) != 1 {
		t.Fatalf("tapPool len=%d, want 1", len(svc.tapPool))
	}
	if svc.tapPool[0].File != nil {
		t.Fatal("newly pooled tap should not keep an open fd")
	}
}

func TestCreatePoolTapDoesNotPoolWhenPrepareFails(t *testing.T) {
	oldNewTap := newTapFunc
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldDestroy := destroyTapFunc
	t.Cleanup(func() {
		newTapFunc = oldNewTap
		cubevsPrepareTAPPolicy = oldPrepareTap
		destroyTapFunc = oldDestroy
	})

	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{
			Name:  tapName(ip.String()),
			Index: 31,
			IP:    ip,
			File:  newTestTapFile(t),
			InUse: true,
		}, nil
	}
	cubevsPrepareTAPPolicy = func(uint32) error { return errors.New("prepare boom") }
	destroyCalls := 0
	destroyTapFunc = func(int) error {
		destroyCalls++
		return nil
	}

	svc := newLifecycleTestService(t)
	usedBefore := svc.allocator.usedIPNum
	if err := svc.createPoolTap(); err == nil {
		t.Fatal("createPoolTap returned nil error")
	}
	if len(svc.tapPool) != 0 {
		t.Fatalf("tapPool len=%d, want 0", len(svc.tapPool))
	}
	if destroyCalls != 1 {
		t.Fatalf("destroyCalls=%d, want 1", destroyCalls)
	}
	if svc.allocator.usedIPNum != usedBefore {
		t.Fatalf("usedIPNum=%d, want %d", svc.allocator.usedIPNum, usedBefore)
	}
}

func TestHandleAbnormalTapsRecoversTapBackToPool(t *testing.T) {
	oldRestore := restoreTapFunc
	oldPrepareTap := cubevsPrepareTAPPolicy
	t.Cleanup(func() {
		restoreTapFunc = oldRestore
		cubevsPrepareTAPPolicy = oldPrepareTap
	})

	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		tap.File = newTestTapFile(t)
		return tap, nil
	}
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }

	svc := newLifecycleTestService(t)
	tap := &tapDevice{
		Name:         "z192.168.0.3",
		Index:        22,
		IP:           net.ParseIP("192.168.0.3").To4(),
		FailureCount: 1,
		LastStage:    abnormalStageRecoverRestore,
		LastError:    "restore failed",
	}
	svc.abnormalTapPool = []*tapDevice{tap}

	svc.handleAbnormalTaps()

	if len(svc.tapPool) != 1 {
		t.Fatalf("tapPool len=%d, want 1", len(svc.tapPool))
	}
	if len(svc.abnormalTapPool) != 0 {
		t.Fatalf("abnormalTapPool len=%d, want 0", len(svc.abnormalTapPool))
	}
	if len(svc.quarantinedTaps) != 0 {
		t.Fatalf("quarantinedTaps len=%d, want 0", len(svc.quarantinedTaps))
	}
	if svc.tapPool[0].FailureCount != 0 || svc.tapPool[0].LastError != "" || svc.tapPool[0].LastStage != "" {
		t.Fatalf("pooled tap recovery metadata not reset: %+v", svc.tapPool[0])
	}
}

func TestHandleAbnormalTapsQuarantinesAfterRepeatedFailures(t *testing.T) {
	oldRestore := restoreTapFunc
	t.Cleanup(func() {
		restoreTapFunc = oldRestore
	})

	restoreTapFunc = func(*tapDevice, int, string, int) (*tapDevice, error) {
		return nil, errors.New("still broken")
	}

	svc := newLifecycleTestService(t)
	tap := &tapDevice{
		Name:         "z192.168.0.4",
		Index:        23,
		IP:           net.ParseIP("192.168.0.4").To4(),
		FailureCount: maxAbnormalRecoveryAttempts - 1,
		LastStage:    abnormalStageRecoverRestore,
	}
	svc.abnormalTapPool = []*tapDevice{tap}

	svc.handleAbnormalTaps()

	if len(svc.tapPool) != 0 {
		t.Fatalf("tapPool len=%d, want 0", len(svc.tapPool))
	}
	if len(svc.abnormalTapPool) != 0 {
		t.Fatalf("abnormalTapPool len=%d, want 0", len(svc.abnormalTapPool))
	}
	quarantined, ok := svc.quarantinedTaps[tap.Name]
	if !ok {
		t.Fatalf("tap %s not quarantined", tap.Name)
	}
	if quarantined.FailureCount != maxAbnormalRecoveryAttempts {
		t.Fatalf("FailureCount=%d, want %d", quarantined.FailureCount, maxAbnormalRecoveryAttempts)
	}
}

func newLifecycleTestService(t *testing.T) *localService {
	t.Helper()

	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}

	return &localService{
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1500},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		quarantinedTaps:   make(map[string]*tapDevice),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
}
