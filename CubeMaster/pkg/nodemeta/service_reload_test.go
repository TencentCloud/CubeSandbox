package nodemeta

import (
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

// newTestService returns a service with a pre-populated in-memory node map,
// suitable for applyReloadResult unit tests (no DB required).
func newTestService(snaps ...*NodeSnapshot) *service {
	s := &service{nodes: make(map[string]*NodeSnapshot, len(snaps))}
	for _, snap := range snaps {
		s.nodes[snap.NodeID] = snap
	}
	return s
}

// TestMergeIncomingHostFacts_PreservesModuleStateOnTransientEmpty locks in the
// fail-open/churn guard: when a heartbeat reports empty KVM module state (a
// transient /sys/module read failure or a kvm.ko mid-reload), the last-known
// module fingerprint + taint must be preserved rather than clobbered — otherwise
// the taint gate silently disables for that window and the DB churns.
func TestMergeIncomingHostFacts_PreservesModuleStateOnTransientEmpty(t *testing.T) {
	prev := &HostFacts{
		CPUIDHash:            "sha256:x86",
		HostKernelRelease:    "5.15.0",
		KVMModuleFingerprint: "sha256:kvmmod",
		KVMModuleTaint:       "O",
	}
	// Fresh heartbeat: static facts present, module state dropped to empty.
	incoming := &HostFacts{
		CPUIDHash:         "sha256:x86",
		HostKernelRelease: "5.15.0",
	}
	merged := mergeIncomingHostFacts(prev, incoming)
	if merged.KVMModuleFingerprint != "sha256:kvmmod" || merged.KVMModuleTaint != "O" {
		t.Fatalf("transient-empty module state must retain previous value, got fp=%q taint=%q",
			merged.KVMModuleFingerprint, merged.KVMModuleTaint)
	}
	// It must not mutate the previous snapshot in place.
	if incoming.KVMModuleTaint != "" {
		t.Fatal("merge must not mutate the incoming facts")
	}
}

// A real module-state change (non-empty incoming) must be adopted, not masked by
// the transient-empty guard.
func TestMergeIncomingHostFacts_AdoptsRealModuleChange(t *testing.T) {
	prev := &HostFacts{KVMModuleFingerprint: "sha256:old", KVMModuleTaint: ""}
	incoming := &HostFacts{KVMModuleFingerprint: "sha256:new", KVMModuleTaint: "E"}
	merged := mergeIncomingHostFacts(prev, incoming)
	if merged.KVMModuleFingerprint != "sha256:new" || merged.KVMModuleTaint != "E" {
		t.Fatalf("real module change must be adopted, got fp=%q taint=%q",
			merged.KVMModuleFingerprint, merged.KVMModuleTaint)
	}
}

// With no previous facts there is nothing to preserve; empty incoming module
// state stays empty.
func TestMergeIncomingHostFacts_NoPrevKeepsIncoming(t *testing.T) {
	incoming := &HostFacts{CPUIDHash: "sha256:x86", HostKernelRelease: "5.15.0"}
	merged := mergeIncomingHostFacts(nil, incoming)
	if merged.KVMModuleFingerprint != "" || merged.KVMModuleTaint != "" {
		t.Fatalf("no-prev merge must keep incoming module state, got fp=%q taint=%q",
			merged.KVMModuleFingerprint, merged.KVMModuleTaint)
	}
}

// A heartbeat that authoritatively scanned /sys/module and found no KVM module
// (KVMModuleScanned=true, empty fingerprint+taint) must CLEAR a previously-known
// taint rather than latch it forever — the module was genuinely unloaded, not a
// read gap.
func TestMergeIncomingHostFacts_ScannedEmptyClearsStaleTaint(t *testing.T) {
	prev := &HostFacts{
		CPUIDHash:            "sha256:x86",
		HostKernelRelease:    "5.15.0",
		KVMModuleFingerprint: "sha256:kvmmod",
		KVMModuleTaint:       "O",
	}
	incoming := &HostFacts{
		CPUIDHash:         "sha256:x86",
		HostKernelRelease: "5.15.0",
		KVMModuleScanned:  true, // authoritative: module absent, not a read gap
	}
	merged := mergeIncomingHostFacts(prev, incoming)
	if merged.KVMModuleFingerprint != "" || merged.KVMModuleTaint != "" {
		t.Fatalf("authoritative empty scan must clear stale module state, got fp=%q taint=%q",
			merged.KVMModuleFingerprint, merged.KVMModuleTaint)
	}
}

// The transient guard still applies when the scan failed (KVMModuleScanned=false
// with empty module state): the previous taint is preserved.
func TestMergeIncomingHostFacts_UnscannedEmptyPreservesTaint(t *testing.T) {
	prev := &HostFacts{KVMModuleFingerprint: "sha256:kvmmod", KVMModuleTaint: "E"}
	incoming := &HostFacts{CPUIDHash: "sha256:x86", KVMModuleScanned: false}
	merged := mergeIncomingHostFacts(prev, incoming)
	if merged.KVMModuleFingerprint != "sha256:kvmmod" || merged.KVMModuleTaint != "E" {
		t.Fatalf("read-gap must preserve previous module state, got fp=%q taint=%q",
			merged.KVMModuleFingerprint, merged.KVMModuleTaint)
	}
}

// KVMModuleScanned is a transient collection signal, never a stored fact: the
// merged result must always zero it so it cannot reach MySQL or a snapshot's
// frozen origin facts.
func TestMergeIncomingHostFacts_ClearsScannedFlag(t *testing.T) {
	for _, scanned := range []bool{true, false} {
		incoming := &HostFacts{
			CPUIDHash:            "sha256:x86",
			KVMModuleFingerprint: "sha256:kvmmod",
			KVMModuleScanned:     scanned,
		}
		if merged := mergeIncomingHostFacts(&HostFacts{}, incoming); merged.KVMModuleScanned {
			t.Errorf("merged facts must never carry KVMModuleScanned (incoming=%v)", scanned)
		}
	}
}

func TestApplyReloadResultUpdatesRegistrationFields(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID:       "node-a",
		Labels:       map[string]string{"zone": "old"},
		InstanceType: "old-type",
		HostIP:       "1.1.1.1",
		GRPCPort:     9000,
		QuotaCPU:     100,
		QuotaMemMB:   512,
	})

	next := map[string]*NodeSnapshot{
		"node-a": {
			NodeID:       "node-a",
			Labels:       map[string]string{"zone": "new", "env": "prod"},
			InstanceType: "new-type",
			HostIP:       "2.2.2.2",
			GRPCPort:     9001,
			QuotaCPU:     200,
			QuotaMemMB:   1024,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-a"]
	s.mu.RUnlock()

	if snap.Labels["zone"] != "new" || snap.Labels["env"] != "prod" {
		t.Fatalf("Labels not updated: %v", snap.Labels)
	}
	if snap.InstanceType != "new-type" {
		t.Fatalf("InstanceType not updated: %s", snap.InstanceType)
	}
	if snap.HostIP != "2.2.2.2" {
		t.Fatalf("HostIP not updated: %s", snap.HostIP)
	}
	if snap.GRPCPort != 9001 {
		t.Fatalf("GRPCPort not updated: %d", snap.GRPCPort)
	}
	if snap.QuotaCPU != 200 {
		t.Fatalf("QuotaCPU not updated: %d", snap.QuotaCPU)
	}
	if snap.QuotaMemMB != 1024 {
		t.Fatalf("QuotaMemMB not updated: %d", snap.QuotaMemMB)
	}
}

func TestApplyReloadResultPreservesInMemoryHeartbeatWhenFresher(t *testing.T) {
	inMemoryTime := time.Now()
	dbTime := inMemoryTime.Add(-5 * time.Second)

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-b",
		HeartbeatTime: inMemoryTime,
		ReportedReady: true,
		Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}},
	})

	next := map[string]*NodeSnapshot{
		"node-b": {
			NodeID:        "node-b",
			HeartbeatTime: dbTime,
			ReportedReady: false,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-b"]
	s.mu.RUnlock()

	if !snap.HeartbeatTime.Equal(inMemoryTime) {
		t.Fatalf("HeartbeatTime regressed: got %v want %v", snap.HeartbeatTime, inMemoryTime)
	}
	if !snap.ReportedReady {
		t.Fatal("ReportedReady regressed: in-memory value should be preserved")
	}
}

func TestApplyReloadResultTakesDBHeartbeatWhenFresher(t *testing.T) {
	oldTime := time.Now().Add(-10 * time.Second)
	newTime := time.Now()

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-c",
		HeartbeatTime: oldTime,
		ReportedReady: false,
	})

	next := map[string]*NodeSnapshot{
		"node-c": {
			NodeID:        "node-c",
			HeartbeatTime: newTime,
			ReportedReady: true,
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-c"]
	s.mu.RUnlock()

	if !snap.HeartbeatTime.Equal(newTime) {
		t.Fatalf("HeartbeatTime not updated: got %v want %v", snap.HeartbeatTime, newTime)
	}
	if !snap.ReportedReady {
		t.Fatal("ReportedReady not updated from DB")
	}
}

func TestApplyReloadResultAdoptsDBHostFactsWhenHeartbeatFresher(t *testing.T) {
	oldTime := time.Now().Add(-10 * time.Second)
	newTime := time.Now()

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-hf",
		HeartbeatTime: oldTime,
		HostFacts:     &HostFacts{CPUIDHash: "sha256:stale"},
	})

	next := map[string]*NodeSnapshot{
		"node-hf": {
			NodeID:        "node-hf",
			HeartbeatTime: newTime,
			HostFacts:     &HostFacts{CPUIDHash: "sha256:fresh"},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-hf"]
	s.mu.RUnlock()

	if snap.HostFacts == nil || snap.HostFacts.CPUIDHash != "sha256:fresh" {
		t.Fatalf("stale in-memory facts must yield to fresher DB facts, got %+v", snap.HostFacts)
	}
}

// TestApplyReloadResultKeepsDirtyInMemoryHostFactsDespiteFresherHeartbeat
// locks in the fix for the status/persist clock skew: the status HeartbeatTime
// advances on every heartbeat, but host facts are only written to the DB when
// they change (and the write may fail). If this replica holds facts pending a
// persist (hostFactsDirty), the DB carries the *older* facts even when its
// status heartbeat is newer, so adopting it would revert the pending value.
func TestApplyReloadResultKeepsDirtyInMemoryHostFactsDespiteFresherHeartbeat(t *testing.T) {
	oldTime := time.Now().Add(-10 * time.Second)
	newTime := time.Now()

	s := newTestService(&NodeSnapshot{
		NodeID:         "node-hf-dirty",
		HeartbeatTime:  oldTime,
		HostFacts:      &HostFacts{CPUIDHash: "sha256:pending"},
		hostFactsDirty: true,
	})

	next := map[string]*NodeSnapshot{
		"node-hf-dirty": {
			NodeID:        "node-hf-dirty",
			HeartbeatTime: newTime, // status clock is newer...
			HostFacts:     &HostFacts{CPUIDHash: "sha256:stale"},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-hf-dirty"]
	s.mu.RUnlock()

	if snap.HostFacts == nil || snap.HostFacts.CPUIDHash != "sha256:pending" {
		t.Fatalf("dirty in-memory facts must not be clobbered by stale DB facts under a newer status heartbeat, got %+v", snap.HostFacts)
	}
}

func TestApplyReloadResultKeepsInMemoryHostFactsWhenHeartbeatNotNewer(t *testing.T) {
	inMemoryTime := time.Now()
	dbTime := inMemoryTime.Add(-5 * time.Second)

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-hf2",
		HeartbeatTime: inMemoryTime,
		HostFacts:     &HostFacts{CPUIDHash: "sha256:fresh"},
	})

	next := map[string]*NodeSnapshot{
		"node-hf2": {
			NodeID:        "node-hf2",
			HeartbeatTime: dbTime,
			HostFacts:     &HostFacts{CPUIDHash: "sha256:stale"},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-hf2"]
	s.mu.RUnlock()

	if snap.HostFacts == nil || snap.HostFacts.CPUIDHash != "sha256:fresh" {
		t.Fatalf("stale DB facts must not clobber fresher in-memory facts, got %+v", snap.HostFacts)
	}
}

func TestApplyReloadResultAdoptsDBHostFactsWhenMemoryEmpty(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID:        "node-hf3",
		HeartbeatTime: time.Now().Add(-time.Minute), // older than DB
	})

	next := map[string]*NodeSnapshot{
		"node-hf3": {
			NodeID:        "node-hf3",
			HeartbeatTime: time.Now(),
			HostFacts:     &HostFacts{CPUIDHash: "sha256:fromdb"},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-hf3"]
	s.mu.RUnlock()

	if snap.HostFacts == nil || snap.HostFacts.CPUIDHash != "sha256:fromdb" {
		t.Fatalf("empty memory must adopt DB facts, got %+v", snap.HostFacts)
	}
}

func TestApplyReloadResultSyncsVersionsForExistingNode(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID: "node-d",
		Versions: []ComponentVersion{
			{Component: "cubelet", Version: "v1.0.0"},
		},
		versionsHash: "oldhash",
	})

	newVersions := []ComponentVersion{
		{Component: "cubelet", Version: "v2.0.0"},
		{Component: "cube-agent", Version: "v1.5.0"},
	}
	next := map[string]*NodeSnapshot{
		"node-d": {
			NodeID:       "node-d",
			Versions:     newVersions,
			versionsHash: versionsHash(newVersions),
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-d"]
	s.mu.RUnlock()

	if len(snap.Versions) != 2 {
		t.Fatalf("Versions length = %d, want 2", len(snap.Versions))
	}
	found := false
	for _, v := range snap.Versions {
		if v.Component == "cubelet" && v.Version == "v2.0.0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("updated cubelet version not found in %v", snap.Versions)
	}
	wantHash := versionsHash(newVersions)
	if snap.versionsHash != wantHash {
		t.Fatalf("versionsHash = %s, want %s", snap.versionsHash, wantHash)
	}
}

func TestApplyReloadResultAddsNewNodeFromDB(t *testing.T) {
	s := newTestService(&NodeSnapshot{NodeID: "node-existing"})

	newTime := time.Now()
	next := map[string]*NodeSnapshot{
		"node-existing": {NodeID: "node-existing"},
		"node-new": {
			NodeID:        "node-new",
			HostIP:        "3.3.3.3",
			Labels:        map[string]string{"region": "us-east"},
			HeartbeatTime: newTime,
			ReportedReady: true,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap, ok := s.nodes["node-new"]
	s.mu.RUnlock()

	if !ok {
		t.Fatal("new node from DB not added to in-memory map")
	}
	if snap.HostIP != "3.3.3.3" {
		t.Fatalf("HostIP = %s, want 3.3.3.3", snap.HostIP)
	}
	if snap.Labels["region"] != "us-east" {
		t.Fatalf("Labels = %v, want region=us-east", snap.Labels)
	}
}

// TestMergeReloadResultReturnsAllTouchedNodes locks in the Option B contract:
// the periodic reload re-syncs EVERY touched node into localcache (node health),
// not only nodes whose cordon state changed. mergeReloadResult must therefore
// return a clone for both an updated existing node and a node newly discovered
// from the DB. The clones must be decoupled from the in-memory map, and must
// carry the derived cordon state so syncLocalcacheNodeHealth conveys it.
func TestMergeReloadResultReturnsAllTouchedNodes(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID: "node-existing",
		Labels: map[string]string{"zone": "old"},
	})

	next := map[string]*NodeSnapshot{
		"node-existing": {
			NodeID: "node-existing",
			Labels: map[string]string{
				"zone":                            "new",
				constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
			},
		},
		"node-new": {
			NodeID: "node-new",
			HostIP: "3.3.3.3",
			Labels: map[string]string{"region": "us-east"},
		},
	}

	syncSnaps, evicted := s.mergeReloadResult(next)

	if len(syncSnaps) != 2 {
		t.Fatalf("mergeReloadResult returned %d snaps, want 2 (every touched node)", len(syncSnaps))
	}
	if len(evicted) != 0 {
		t.Fatalf("mergeReloadResult returned unexpected evictions: %v", evicted)
	}

	byID := make(map[string]*NodeSnapshot, len(syncSnaps))
	for _, snap := range syncSnaps {
		byID[snap.NodeID] = snap
	}
	existing, ok := byID["node-existing"]
	if !ok {
		t.Fatal("updated existing node missing from sync set")
	}
	if _, ok := byID["node-new"]; !ok {
		t.Fatal("new-from-DB node missing from sync set")
	}

	// Clone reflects the merged (new) value and carries derived cordon state.
	if existing.Labels["zone"] != "new" {
		t.Fatalf("returned clone not merged: zone=%s want new", existing.Labels["zone"])
	}
	if !existing.SchedulingDisabled {
		t.Fatal("returned clone must carry SchedulingDisabled derived from labels")
	}

	// Clone is decoupled from the in-memory map.
	existing.Labels["zone"] = "mutated"
	s.mu.RLock()
	stored := s.nodes["node-existing"].Labels["zone"]
	s.mu.RUnlock()
	if stored != "new" {
		t.Fatalf("mutating returned clone leaked into in-memory map: stored zone=%s", stored)
	}
}

// TestApplyReloadResultSyncsNodeHealthWhenReady covers the core behavioral fix:
// once s.ready is true, applyReloadResult must push EVERY touched node (updated
// existing + newly discovered from DB) into localcache via the node-health sync
// hook. The hook is swapped for a spy so the assertion needs no localcache
// singleton. This is the path that lets a non-owner replica's DB fallback match
// a ready template replica and avoid the 130400 failure.
func TestApplyReloadResultSyncsNodeHealthWhenReady(t *testing.T) {
	var synced []string
	orig := syncNodeHealthFn
	syncNodeHealthFn = func(snap *NodeSnapshot) { synced = append(synced, snap.NodeID) }
	defer func() { syncNodeHealthFn = orig }()

	s := newTestService(&NodeSnapshot{
		NodeID: "node-existing",
		Labels: map[string]string{"zone": "old"},
	})
	s.ready = true

	next := map[string]*NodeSnapshot{
		"node-existing": {NodeID: "node-existing", Labels: map[string]string{"zone": "new"}},
		"node-new":      {NodeID: "node-new", HostIP: "3.3.3.3"},
	}
	s.applyReloadResult(next)

	sort.Strings(synced)
	if len(synced) != 2 || synced[0] != "node-existing" || synced[1] != "node-new" {
		t.Fatalf("node-health sync not invoked for every touched node: got %v want [node-existing node-new]", synced)
	}
}

// TestApplyReloadResultSkipsSyncBeforeReady locks in the Init-ordering safety:
// the very first reload runs before s.ready is set and before localcache.Init,
// so it must NOT touch localcache (the caches do not exist yet).
func TestApplyReloadResultSkipsSyncBeforeReady(t *testing.T) {
	called := false
	orig := syncNodeHealthFn
	syncNodeHealthFn = func(*NodeSnapshot) { called = true }
	defer func() { syncNodeHealthFn = orig }()

	s := newTestService(&NodeSnapshot{NodeID: "node-a"})
	// s.ready defaults to false (mirrors the pre-Init initial reload).

	s.applyReloadResult(map[string]*NodeSnapshot{"node-a": {NodeID: "node-a"}})

	if called {
		t.Fatal("node-health sync must be skipped before s.ready is set")
	}
}
