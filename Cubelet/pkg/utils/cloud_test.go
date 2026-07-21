// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"net"
	"sync"
	"testing"
)

func TestGetInstanceID(t *testing.T) {
	SkipCI(t)

	instanceID, err := GetInstanceID()
	if err != nil {
		t.Fatalf("GetInstanceID returned an error: %v", err)
	}
	if instanceID == "" {
		t.Error("GetInstanceID returned an empty string")
	}
}

func TestGetLocalIpv4(t *testing.T) {
	SkipCI(t)

	localIpv4, err := GetLocalIpv4()
	if err != nil {
		t.Fatalf("GetLocalIpv4 returned an error: %v", err)
	}

	if net.ParseIP(localIpv4) == nil {
		t.Errorf("GetLocalIpv4 returned an invalid IP address: %s", localIpv4)
	}
}

func TestGetRegion(t *testing.T) {
	SkipCI(t)

	region, err := GetRegion()
	if err != nil {
		t.Fatalf("GetRegion returned an error: %v", err)
	}
	if region != "" {
		t.Errorf("GetRegion returned an unexpected value: %s", region)
	}
}

func TestGetShortInstanceType(t *testing.T) {
	instanceType, err := GetShortInstanceType()
	if err != nil {
		t.Fatalf("GetShortInstanceType returned an error: %v", err)
	}
	if instanceType != "cubebox" {
		t.Fatalf("expected instance type cubebox, got %s", instanceType)
	}
}

func TestGetHostIdentityUsesEnvOverride(t *testing.T) {
	resetHostIdentityCache()
	t.Cleanup(resetHostIdentityCache)
	t.Setenv("CUBE_SANDBOX_NODE_ID", "")
	t.Setenv("CUBE_SANDBOX_ENDPOINT_IP", "")
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.20.30.40")

	identity, err := GetHostIdentity()
	if err != nil {
		t.Fatalf("GetHostIdentity returned an error: %v", err)
	}
	if identity.InstanceID != "10.20.30.40" {
		t.Fatalf("expected instance id from env override, got %s", identity.InstanceID)
	}
	if identity.LocalIPv4 != "10.20.30.40" {
		t.Fatalf("expected local ip from env override, got %s", identity.LocalIPv4)
	}
}

func TestGetHostIdentitySeparatesNodeIDAndEndpointIP(t *testing.T) {
	resetHostIdentityCache()
	t.Cleanup(resetHostIdentityCache)
	t.Setenv("CUBE_SANDBOX_NODE_ID", "node-a")
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.20.30.40")
	t.Setenv("CUBE_SANDBOX_ENDPOINT_IP", "10.244.1.5")

	identity, err := GetHostIdentity()
	if err != nil {
		t.Fatalf("GetHostIdentity returned an error: %v", err)
	}
	if identity.InstanceID != "node-a" {
		t.Fatalf("expected InstanceID=node-a, got %s", identity.InstanceID)
	}
	if identity.LocalIPv4 != "10.244.1.5" {
		t.Fatalf("expected LocalIPv4=10.244.1.5, got %s", identity.LocalIPv4)
	}
}

func resetHostIdentityCache() {
	hostIdentityOnce = sync.Once{}
	hostIdentity = HostIdentity{}
	hostIdentityErr = nil
}
