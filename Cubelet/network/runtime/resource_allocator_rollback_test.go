// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import "testing"

func TestReserveRollbackKeepsPortsFromEarlierReservation(t *testing.T) {
	b := newTestPortBinder()
	const owner = "sandbox-A"

	if _, err := b.Reserve(owner, []PortMapping{
		{ContainerPort: 8080, HostPort: 20500},
	}, "10.0.0.1"); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	if got := b.assigned[20500]; got != owner {
		t.Fatalf("after first Reserve, assigned[20500] = %q, want %q", got, owner)
	}

	_, err := b.Reserve(owner, []PortMapping{
		{ContainerPort: 8080, HostPort: 20500},
		{ContainerPort: 0, HostPort: 20501},
	}, "10.0.0.1")
	if err == nil {
		t.Fatal("second Reserve was expected to fail on the invalid container port")
	}

	if got := b.assigned[20500]; got != owner {
		t.Fatalf("rollback revoked a port from the earlier reservation: assigned[20500] = %q, want %q", got, owner)
	}
	if _, ok := b.owners[owner][20500]; !ok {
		t.Fatal("rollback removed 20500 from the owner set")
	}
	if _, leaked := b.assigned[20501]; leaked {
		t.Fatal("the failed reservation leaked port 20501")
	}
}

func TestReserveRollbackReleasesOnlyItsOwnPorts(t *testing.T) {
	b := newTestPortBinder()

	if _, err := b.Reserve("A", []PortMapping{{ContainerPort: 80, HostPort: 20500}}, "ip"); err != nil {
		t.Fatalf("Reserve for A: %v", err)
	}

	if _, err := b.Reserve("B", []PortMapping{
		{ContainerPort: 81, HostPort: 20501},
		{ContainerPort: 0, HostPort: 20502},
	}, "ip"); err == nil {
		t.Fatal("Reserve for B was expected to fail")
	}

	if got := b.assigned[20500]; got != "A" {
		t.Fatalf("A's port was collaterally released: assigned[20500] = %q", got)
	}
	if _, leaked := b.assigned[20501]; leaked {
		t.Fatal("B's partial port leaked")
	}
}

func TestReserveRollbackReleasesNewlyAcquiredPorts(t *testing.T) {
	b := newTestPortBinder()
	const owner = "sandbox-A"

	_, err := b.Reserve(owner, []PortMapping{
		{ContainerPort: 80, HostPort: 20600},
		{ContainerPort: 0, HostPort: 20601},
	}, "ip")
	if err == nil {
		t.Fatal("Reserve was expected to fail")
	}
	if _, leaked := b.assigned[20600]; leaked {
		t.Fatal("a port acquired by the failed call was not released")
	}
	if _, present := b.owners[owner]; present {
		t.Fatal("the owner entry was left behind after a fully-rolled-back reservation")
	}
}

func TestReserveAutomaticAllocationStillWorks(t *testing.T) {
	b := newTestPortBinder()

	got, err := b.Reserve("A", []PortMapping{
		{ContainerPort: 80},
		{ContainerPort: 81},
	}, "10.0.0.1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mappings, want 2", len(got))
	}
	if got[0].HostPort == got[1].HostPort {
		t.Fatalf("automatic allocation reused the same host port %d", got[0].HostPort)
	}
	for _, m := range got {
		if m.HostPort < int32(portMin) || m.HostPort > int32(portMax) {
			t.Fatalf("host port %d outside the managed range", m.HostPort)
		}
	}
}
