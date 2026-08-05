package cubevs

import (
	"errors"
	"testing"

	"github.com/cilium/ebpf"
)

type fakePortMap struct {
	remote map[uint16]MVMPort
	local  map[MVMPort]uint16

	lookupErr   error
	updateErr   error
	deleteErr   error
	updateFunc  func(key, value any, flags ebpf.MapUpdateFlags) error
	updateCalls int
}

func (m *fakePortMap) Lookup(key, valueOut interface{}) error {
	if m.lookupErr != nil {
		return m.lookupErr
	}
	switch typedKey := key.(type) {
	case *uint16:
		value, ok := m.remote[*typedKey]
		if !ok {
			return ebpf.ErrKeyNotExist
		}
		*valueOut.(*MVMPort) = value
		return nil
	case *MVMPort:
		value, ok := m.local[*typedKey]
		if !ok {
			return ebpf.ErrKeyNotExist
		}
		*valueOut.(*uint16) = value
		return nil
	default:
		panic("unexpected lookup key type")
	}
}

func (m *fakePortMap) Update(key, value any, flags ebpf.MapUpdateFlags) error {
	m.updateCalls++
	if m.updateFunc != nil {
		if err := m.updateFunc(key, value, flags); err != nil {
			return err
		}
	}
	if m.updateErr != nil {
		return m.updateErr
	}
	switch typedKey := key.(type) {
	case *uint16:
		if _, exists := m.remote[*typedKey]; flags == ebpf.UpdateNoExist && exists {
			return ebpf.ErrKeyExist
		}
		m.remote[*typedKey] = *value.(*MVMPort)
	case *MVMPort:
		if _, exists := m.local[*typedKey]; flags == ebpf.UpdateNoExist && exists {
			return ebpf.ErrKeyExist
		}
		m.local[*typedKey] = *value.(*uint16)
	default:
		panic("unexpected update key type")
	}
	return nil
}

func (m *fakePortMap) Delete(key interface{}) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	switch typedKey := key.(type) {
	case *uint16:
		if _, ok := m.remote[*typedKey]; !ok {
			return ebpf.ErrKeyNotExist
		}
		delete(m.remote, *typedKey)
	case *MVMPort:
		if _, ok := m.local[*typedKey]; !ok {
			return ebpf.ErrKeyNotExist
		}
		delete(m.local, *typedKey)
	default:
		panic("unexpected delete key type")
	}
	return nil
}

func TestAddPortMappingRollsBackNewRemoteEntry(t *testing.T) {
	updateErr := errors.New("local update failed")
	remote := &fakePortMap{remote: make(map[uint16]MVMPort)}
	local := &fakePortMap{
		local:     make(map[MVMPort]uint16),
		updateErr: updateErr,
	}

	err := addPortMapping(remote, local, 12, 80, 20080)
	if !errors.Is(err, updateErr) {
		t.Fatalf("addPortMapping error = %v, want %v", err, updateErr)
	}
	if len(remote.remote) != 0 {
		t.Fatalf("remote map after rollback = %#v, want empty", remote.remote)
	}
}

func TestAddPortMappingRejectsRemoteConflictWithoutMutation(t *testing.T) {
	hostPort := htons(20080)
	previous := MVMPort{Ifindex: 7, ListenPort: htons(8080)}
	remote := &fakePortMap{remote: map[uint16]MVMPort{hostPort: previous}}
	local := &fakePortMap{local: make(map[MVMPort]uint16)}

	err := addPortMapping(remote, local, 12, 80, 20080)
	if err == nil {
		t.Fatal("addPortMapping returned nil for a remote owner conflict")
	}
	if got := remote.remote[hostPort]; got != previous || len(local.local) != 0 {
		t.Fatalf("maps changed after remote conflict: remote=%#v local=%#v", remote.remote, local.local)
	}
	if remote.updateCalls != 0 || local.updateCalls != 0 {
		t.Fatalf("updates after preflight conflict: remote=%d local=%d", remote.updateCalls, local.updateCalls)
	}
}

func TestAddPortMappingReportsRollbackFailure(t *testing.T) {
	updateErr := errors.New("local update failed")
	rollbackErr := errors.New("remote delete failed")
	remote := &fakePortMap{
		remote:    make(map[uint16]MVMPort),
		deleteErr: rollbackErr,
	}
	local := &fakePortMap{
		local:     make(map[MVMPort]uint16),
		updateErr: updateErr,
	}

	err := addPortMapping(remote, local, 12, 80, 20080)
	if !errors.Is(err, updateErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("addPortMapping error = %v, want joined update and rollback errors", err)
	}
}

func TestAddPortMappingRejectsLocalConflictBeforeRemoteMutation(t *testing.T) {
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	otherHostPort := htons(20081)
	remote := &fakePortMap{remote: make(map[uint16]MVMPort)}
	local := &fakePortMap{local: map[MVMPort]uint16{desired: otherHostPort}}

	err := addPortMapping(remote, local, 12, 80, 20080)
	if err == nil {
		t.Fatal("addPortMapping returned nil for a local owner conflict")
	}
	if len(remote.remote) != 0 || local.local[desired] != otherHostPort {
		t.Fatalf("maps changed after local conflict: remote=%#v local=%#v", remote.remote, local.local)
	}
	if remote.updateCalls != 0 || local.updateCalls != 0 {
		t.Fatalf("updates after preflight conflict: remote=%d local=%d", remote.updateCalls, local.updateCalls)
	}
}

func TestAddPortMappingExactPairIsIdempotent(t *testing.T) {
	updateErr := errors.New("update should not run")
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	hostPort := htons(20080)
	remote := &fakePortMap{
		remote:    map[uint16]MVMPort{hostPort: desired},
		updateErr: updateErr,
	}
	local := &fakePortMap{
		local:     map[MVMPort]uint16{desired: hostPort},
		updateErr: updateErr,
	}

	if err := addPortMapping(remote, local, 12, 80, 20080); err != nil {
		t.Fatalf("idempotent addPortMapping error = %v", err)
	}
	if remote.updateCalls != 0 || local.updateCalls != 0 {
		t.Fatalf("idempotent add performed updates: remote=%d local=%d", remote.updateCalls, local.updateCalls)
	}
}

func TestAddPortMappingRepairsMissingSidesWithoutOverwriting(t *testing.T) {
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	hostPort := htons(20080)

	tests := []struct {
		name              string
		remote            map[uint16]MVMPort
		local             map[MVMPort]uint16
		wantRemoteUpdates int
		wantLocalUpdates  int
	}{
		{
			name:              "both missing",
			remote:            make(map[uint16]MVMPort),
			local:             make(map[MVMPort]uint16),
			wantRemoteUpdates: 1,
			wantLocalUpdates:  1,
		},
		{
			name:              "remote missing",
			remote:            make(map[uint16]MVMPort),
			local:             map[MVMPort]uint16{desired: hostPort},
			wantRemoteUpdates: 1,
			wantLocalUpdates:  0,
		},
		{
			name:              "local missing",
			remote:            map[uint16]MVMPort{hostPort: desired},
			local:             make(map[MVMPort]uint16),
			wantRemoteUpdates: 0,
			wantLocalUpdates:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakePortMap{remote: tt.remote}
			local := &fakePortMap{local: tt.local}
			remote.updateFunc = requireUpdateNoExist(t)
			local.updateFunc = requireUpdateNoExist(t)

			if err := addPortMapping(remote, local, 12, 80, 20080); err != nil {
				t.Fatalf("addPortMapping error = %v", err)
			}
			if got := remote.remote[hostPort]; got != desired {
				t.Fatalf("remote mapping = %#v, want %#v", got, desired)
			}
			if got := local.local[desired]; got != hostPort {
				t.Fatalf("local host port = %d, want %d", ntohs(got), ntohs(hostPort))
			}
			if remote.updateCalls != tt.wantRemoteUpdates || local.updateCalls != tt.wantLocalUpdates {
				t.Fatalf(
					"update calls remote/local = %d/%d, want %d/%d",
					remote.updateCalls,
					local.updateCalls,
					tt.wantRemoteUpdates,
					tt.wantLocalUpdates,
				)
			}
		})
	}
}

func requireUpdateNoExist(t *testing.T) func(key, value any, flags ebpf.MapUpdateFlags) error {
	t.Helper()
	return func(_ any, _ any, flags ebpf.MapUpdateFlags) error {
		if flags != ebpf.UpdateNoExist {
			t.Fatalf("update flags = %d, want UpdateNoExist", flags)
		}
		return nil
	}
}

func TestAddPortMappingAcceptsConcurrentExactRemoteInsert(t *testing.T) {
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	hostPort := htons(20080)
	remote := &fakePortMap{remote: make(map[uint16]MVMPort)}
	remote.updateFunc = func(key, _ any, flags ebpf.MapUpdateFlags) error {
		if flags != ebpf.UpdateNoExist {
			t.Fatalf("remote update flags = %d, want UpdateNoExist", flags)
		}
		remote.remote[*key.(*uint16)] = desired
		return ebpf.ErrKeyExist
	}
	local := &fakePortMap{local: make(map[MVMPort]uint16)}

	if err := addPortMapping(remote, local, 12, 80, 20080); err != nil {
		t.Fatalf("addPortMapping error = %v", err)
	}
	if got := remote.remote[hostPort]; got != desired {
		t.Fatalf("remote mapping = %#v, want %#v", got, desired)
	}
	if got := local.local[desired]; got != hostPort {
		t.Fatalf("local host port = %d, want %d", ntohs(got), ntohs(hostPort))
	}
}

func TestAddPortMappingRejectsConcurrentRemoteConflict(t *testing.T) {
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	other := MVMPort{Ifindex: 13, ListenPort: htons(81)}
	hostPort := htons(20080)
	remote := &fakePortMap{remote: make(map[uint16]MVMPort)}
	remote.updateFunc = func(key, _ any, _ ebpf.MapUpdateFlags) error {
		remote.remote[*key.(*uint16)] = other
		return ebpf.ErrKeyExist
	}
	local := &fakePortMap{local: make(map[MVMPort]uint16)}

	err := addPortMapping(remote, local, desired.Ifindex, ntohs(desired.ListenPort), ntohs(hostPort))
	if err == nil {
		t.Fatal("addPortMapping returned nil for a concurrent remote conflict")
	}
	if got := remote.remote[hostPort]; got != other {
		t.Fatalf("concurrent remote mapping = %#v, want %#v", got, other)
	}
	if len(local.local) != 0 {
		t.Fatalf("local map changed after concurrent remote conflict: %#v", local.local)
	}
}

func TestAddPortMappingRollsBackRemoteAfterConcurrentLocalConflict(t *testing.T) {
	desired := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	hostPort := htons(20080)
	otherHostPort := htons(20081)
	remote := &fakePortMap{remote: make(map[uint16]MVMPort)}
	local := &fakePortMap{local: make(map[MVMPort]uint16)}
	local.updateFunc = func(key, _ any, _ ebpf.MapUpdateFlags) error {
		local.local[*key.(*MVMPort)] = otherHostPort
		return ebpf.ErrKeyExist
	}

	err := addPortMapping(remote, local, desired.Ifindex, ntohs(desired.ListenPort), ntohs(hostPort))
	if err == nil {
		t.Fatal("addPortMapping returned nil for a concurrent local conflict")
	}
	if len(remote.remote) != 0 {
		t.Fatalf("new remote mapping was not rolled back: %#v", remote.remote)
	}
	if got := local.local[desired]; got != otherHostPort {
		t.Fatalf("concurrent local mapping = %d, want %d", ntohs(got), ntohs(otherHostPort))
	}
}

func TestDelPortMappingDeletesOnlyMatchingEntries(t *testing.T) {
	expected := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	expectedHostPort := htons(20080)
	other := MVMPort{Ifindex: 13, ListenPort: htons(81)}
	otherHostPort := htons(20081)

	tests := []struct {
		name            string
		local           map[MVMPort]uint16
		remote          map[uint16]MVMPort
		wantLocalCount  int
		wantRemoteCount int
	}{
		{
			name:            "matching pair",
			local:           map[MVMPort]uint16{expected: expectedHostPort},
			remote:          map[uint16]MVMPort{expectedHostPort: expected},
			wantLocalCount:  0,
			wantRemoteCount: 0,
		},
		{
			name:            "mismatched entries",
			local:           map[MVMPort]uint16{expected: otherHostPort},
			remote:          map[uint16]MVMPort{expectedHostPort: other},
			wantLocalCount:  1,
			wantRemoteCount: 1,
		},
		{
			name:            "local only",
			local:           map[MVMPort]uint16{expected: expectedHostPort},
			remote:          make(map[uint16]MVMPort),
			wantLocalCount:  0,
			wantRemoteCount: 0,
		},
		{
			name:            "remote only",
			local:           make(map[MVMPort]uint16),
			remote:          map[uint16]MVMPort{expectedHostPort: expected},
			wantLocalCount:  0,
			wantRemoteCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := &fakePortMap{local: tt.local}
			remote := &fakePortMap{remote: tt.remote}

			if err := delPortMapping(local, remote, 12, 80, 20080); err != nil {
				t.Fatalf("delPortMapping error = %v", err)
			}
			if got := len(local.local); got != tt.wantLocalCount {
				t.Fatalf("local map length = %d, want %d", got, tt.wantLocalCount)
			}
			if got := len(remote.remote); got != tt.wantRemoteCount {
				t.Fatalf("remote map length = %d, want %d", got, tt.wantRemoteCount)
			}
		})
	}
}

func TestDelPortMappingLooksUpBothMapsBeforeDeleting(t *testing.T) {
	lookupErr := errors.New("remote lookup failed")
	expected := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	expectedHostPort := htons(20080)
	local := &fakePortMap{local: map[MVMPort]uint16{expected: expectedHostPort}}
	remote := &fakePortMap{
		remote:    make(map[uint16]MVMPort),
		lookupErr: lookupErr,
	}

	err := delPortMapping(local, remote, 12, 80, 20080)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("delPortMapping error = %v, want %v", err, lookupErr)
	}
	if got := local.local[expected]; got != expectedHostPort {
		t.Fatalf("local mapping was deleted before remote lookup: got %d, want %d", got, expectedHostPort)
	}
}

func TestDelPortMappingAttemptsBothDeletes(t *testing.T) {
	deleteErr := errors.New("local delete failed")
	expected := MVMPort{Ifindex: 12, ListenPort: htons(80)}
	expectedHostPort := htons(20080)
	local := &fakePortMap{
		local:     map[MVMPort]uint16{expected: expectedHostPort},
		deleteErr: deleteErr,
	}
	remote := &fakePortMap{remote: map[uint16]MVMPort{expectedHostPort: expected}}

	err := delPortMapping(local, remote, 12, 80, 20080)
	if !errors.Is(err, deleteErr) {
		t.Fatalf("delPortMapping error = %v, want %v", err, deleteErr)
	}
	if _, ok := remote.remote[expectedHostPort]; ok {
		t.Fatal("remote mapping was not deleted after local delete failed")
	}
}

func TestDeletePortMappingTuplesByIfindexToleratesOneSidedAndConflictingEntries(t *testing.T) {
	const (
		targetIfindex = uint32(12)
		otherIfindex  = uint32(13)
	)

	matched := MVMPort{Ifindex: targetIfindex, ListenPort: htons(80)}
	remoteOnly := MVMPort{Ifindex: targetIfindex, ListenPort: htons(81)}
	localOnly := MVMPort{Ifindex: targetIfindex, ListenPort: htons(82)}
	targetRemoteConflict := MVMPort{Ifindex: targetIfindex, ListenPort: htons(83)}
	targetLocalConflict := MVMPort{Ifindex: targetIfindex, ListenPort: htons(84)}
	otherLocalConflict := MVMPort{Ifindex: otherIfindex, ListenPort: htons(90)}
	otherRemoteConflict := MVMPort{Ifindex: otherIfindex, ListenPort: htons(91)}
	unrelated := MVMPort{Ifindex: otherIfindex, ListenPort: htons(92)}

	matchedHostPort := htons(20080)
	remoteOnlyHostPort := htons(20081)
	localOnlyHostPort := htons(20082)
	remoteConflictHostPort := htons(20083)
	localConflictHostPort := htons(20084)
	unrelatedHostPort := htons(20085)

	remote := &fakePortMap{remote: map[uint16]MVMPort{
		matchedHostPort:        matched,
		remoteOnlyHostPort:     remoteOnly,
		remoteConflictHostPort: targetRemoteConflict,
		localConflictHostPort:  otherRemoteConflict,
		unrelatedHostPort:      unrelated,
	}}
	local := &fakePortMap{local: map[MVMPort]uint16{
		matched:             matchedHostPort,
		localOnly:           localOnlyHostPort,
		otherLocalConflict:  remoteConflictHostPort,
		targetLocalConflict: localConflictHostPort,
		unrelated:           unrelatedHostPort,
	}}

	tuples := make(map[portMappingTuple]struct{})
	for hostPort, mvmPort := range remote.remote {
		addPortMappingTupleForIfindex(tuples, targetIfindex, mvmPort, hostPort)
	}
	for mvmPort, hostPort := range local.local {
		addPortMappingTupleForIfindex(tuples, targetIfindex, mvmPort, hostPort)
	}
	if got, want := len(tuples), 5; got != want {
		t.Fatalf("target tuple count = %d, want %d", got, want)
	}

	if err := deletePortMappingTuples(local, remote, tuples); err != nil {
		t.Fatalf("deletePortMappingTuples error = %v", err)
	}
	for hostPort, mvmPort := range remote.remote {
		if mvmPort.Ifindex == targetIfindex {
			t.Errorf("target remote mapping remains: host port %d, mapping %#v", ntohs(hostPort), mvmPort)
		}
	}
	for mvmPort, hostPort := range local.local {
		if mvmPort.Ifindex == targetIfindex {
			t.Errorf("target local mapping remains: mapping %#v, host port %d", mvmPort, ntohs(hostPort))
		}
	}

	if got := remote.remote[localConflictHostPort]; got != otherRemoteConflict {
		t.Errorf("unrelated conflicting remote mapping = %#v, want %#v", got, otherRemoteConflict)
	}
	if got := local.local[otherLocalConflict]; got != remoteConflictHostPort {
		t.Errorf("unrelated conflicting local host port = %d, want %d", ntohs(got), ntohs(remoteConflictHostPort))
	}
	if got := remote.remote[unrelatedHostPort]; got != unrelated {
		t.Errorf("unrelated remote mapping = %#v, want %#v", got, unrelated)
	}
	if got := local.local[unrelated]; got != unrelatedHostPort {
		t.Errorf("unrelated local host port = %d, want %d", ntohs(got), ntohs(unrelatedHostPort))
	}
}

func TestAddListedPortMappingRejectsConflict(t *testing.T) {
	result := map[uint16]MVMPort{
		20080: {Ifindex: 12, ListenPort: 80},
	}

	if err := addListedPortMapping(result, 20080, MVMPort{Ifindex: 12, ListenPort: 80}); err != nil {
		t.Fatalf("matching duplicate returned error: %v", err)
	}
	if err := addListedPortMapping(result, 20081, MVMPort{Ifindex: 13, ListenPort: 81}); err != nil {
		t.Fatalf("one-sided mapping returned error: %v", err)
	}
	if err := addListedPortMapping(result, 20080, MVMPort{Ifindex: 14, ListenPort: 82}); err == nil {
		t.Fatal("conflicting mapping returned nil error")
	}
	if got := result[20080]; got != (MVMPort{Ifindex: 12, ListenPort: 80}) {
		t.Fatalf("conflicting mapping overwrote result: got %#v", got)
	}
}
