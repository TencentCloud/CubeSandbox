// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newHostFactsQueryDB returns an in-memory sqlite DB with the registration and
// status tables migrated, wired as the nodemeta package DB for the test.
func newHostFactsQueryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:hostfacts_query_"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.NodeRegistration{}, &models.NodeStatus{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	orig := global
	global = &service{db: db, nodes: map[string]*NodeSnapshot{}}
	t.Cleanup(func() { global = orig })
	return db
}

// seedNode inserts a registration + status row. fresh controls whether the
// heartbeat is within the health timeout.
func seedNode(t *testing.T, db *gorm.DB, nodeID, ip string, facts *HostFacts, healthy, fresh bool) {
	t.Helper()
	reg := &models.NodeRegistration{
		NodeID:        nodeID,
		HostIP:        ip,
		HostFactsJSON: marshalHostFacts(facts),
	}
	applyHostFactColumns(reg, facts)
	if err := db.Table("t_cube_node_registration").Create(reg).Error; err != nil {
		t.Fatalf("seed reg %s: %v", nodeID, err)
	}
	hb := time.Now().Unix()
	if !fresh {
		hb = time.Now().Add(-2 * healthTimeout()).Unix()
	}
	st := &models.NodeStatus{NodeID: nodeID, HeartbeatUnix: hb, Healthy: healthy}
	if err := db.Table("t_cube_node_status").Create(st).Error; err != nil {
		t.Fatalf("seed status %s: %v", nodeID, err)
	}
}

func x86Facts() *HostFacts {
	return &HostFacts{
		CPUVendor:         "GenuineIntel",
		CPUModel:          "Xeon 8255C",
		CPUIDHash:         "sha256:x86",
		HostKernelRelease: "5.15.0",
	}
}

// armFacts mirrors an aarch64 node: no vendor/model, but the two required keys
// (cpuid_hash folds the ARM core identity, kernel release) are populated.
func armFacts() *HostFacts {
	return &HostFacts{
		CPUIDHash:         "sha256:arm-neoverse-n1",
		HostKernelRelease: "5.15.0-arm",
	}
}

func TestQueryHostFactCandidates_MatchesRequiredKeys(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "match", "10.0.0.1", x86Facts(), true, true)

	mismatch := x86Facts()
	mismatch.CPUIDHash = "sha256:other"
	seedNode(t, db, "cpuid-mismatch", "10.0.0.2", mismatch, true, true)

	relMismatch := x86Facts()
	relMismatch.HostKernelRelease = "6.1.0"
	seedNode(t, db, "release-mismatch", "10.0.0.3", relMismatch, true, true)

	got, err := QueryHostFactCandidates(context.Background(), "sha256:x86", "5.15.0", false)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "match" || got[0].HostIP != "10.0.0.1" {
		t.Fatalf("want only 'match', got %+v", got)
	}
	if got[0].HostFacts == nil || got[0].HostFacts.CPUIDHash != "sha256:x86" {
		t.Errorf("candidate must carry decoded facts: %+v", got[0].HostFacts)
	}
}

// aarch64: cpu_vendor/cpu_model are empty, but the required-key predicate still
// discriminates on cpuid_hash + host_kernel_release, so an ARM node is matched.
func TestQueryHostFactCandidates_ARMMatchesOnRequiredKeys(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "arm-ok", "10.0.0.9", armFacts(), true, true)
	// A different ARM core (distinct cpuid_hash) must NOT match.
	otherArm := armFacts()
	otherArm.CPUIDHash = "sha256:arm-kunpeng"
	seedNode(t, db, "arm-other", "10.0.0.10", otherArm, true, true)

	got, err := QueryHostFactCandidates(context.Background(), "sha256:arm-neoverse-n1", "5.15.0-arm", false)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "arm-ok" {
		t.Fatalf("ARM node must match on required keys only, got %+v", got)
	}
	if got[0].HostFacts.CPUVendor != "" {
		t.Errorf("ARM node has no vendor, got %q", got[0].HostFacts.CPUVendor)
	}
}

func TestQueryHostFactCandidates_ExcludesUnhealthyAndStale(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "healthy", "10.0.0.1", x86Facts(), true, true)
	seedNode(t, db, "unhealthy", "10.0.0.2", x86Facts(), false, true)
	seedNode(t, db, "stale", "10.0.0.3", x86Facts(), true, false)

	got, err := QueryHostFactCandidates(context.Background(), "sha256:x86", "5.15.0", false)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "healthy" {
		t.Fatalf("only the fresh healthy node must be returned, got %+v", got)
	}
}

// matchAll drops the required-key predicate so a diagnostic caller sees every
// healthy node with facts (including required-key mismatches).
func TestQueryHostFactCandidates_MatchAllReturnsEveryHealthy(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "a", "10.0.0.1", x86Facts(), true, true)
	other := x86Facts()
	other.CPUIDHash = "sha256:other"
	seedNode(t, db, "b", "10.0.0.2", other, true, true)
	seedNode(t, db, "unhealthy", "10.0.0.3", x86Facts(), false, true)

	got, err := QueryHostFactCandidates(context.Background(), "sha256:x86", "5.15.0", true)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matchAll must return both healthy nodes regardless of keys, got %d: %+v", len(got), got)
	}
}

// A node whose registration row exists but carries no facts (empty json) must be
// skipped even though its status row is healthy.
func TestQueryHostFactCandidates_SkipsNodesWithoutFacts(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "nofacts", "10.0.0.1", nil, true, true)

	got, err := QueryHostFactCandidates(context.Background(), "sha256:x86", "5.15.0", true)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("node without facts must be skipped, got %+v", got)
	}
}

// persistHostFacts must treat "0 rows updated" (no registration row) as a
// failure so the dirty-retry loop keeps the write in flight; a nil GORM error
// with RowsAffected==0 previously disarmed the retry and left the DB stale.
func TestPersistHostFacts_NoRegistrationRowIsError(t *testing.T) {
	db := newHostFactsQueryDB(t)
	// No registration row for this node.
	err := global.persistHostFacts(context.Background(), "ghost-node", x86Facts())
	if err == nil {
		t.Fatal("persistHostFacts must return an error when no registration row matches")
	}
	_ = db
}

func TestPersistHostFacts_ExistingRowSucceeds(t *testing.T) {
	db := newHostFactsQueryDB(t)
	seedNode(t, db, "real-node", "10.0.0.9", nil, true, true)

	if err := global.persistHostFacts(context.Background(), "real-node", x86Facts()); err != nil {
		t.Fatalf("persistHostFacts on existing row: %v", err)
	}
	facts, ok := GetPersistedNodeHostFacts(context.Background(), "real-node")
	if !ok || facts == nil || facts.CPUIDHash != "sha256:x86" {
		t.Fatalf("persisted facts not readable back, got ok=%v facts=%+v", ok, facts)
	}
}

// GetPersistedNodeHostFacts reads straight from the registration row regardless
// of live health, so snapshot-create can backfill origin facts when the node is
// momentarily unhealthy instead of freezing origin_fingerprint_unknown.
func TestGetPersistedNodeHostFacts_IgnoresHealth(t *testing.T) {
	db := newHostFactsQueryDB(t)
	// Unhealthy + stale heartbeat, but facts are on disk.
	seedNode(t, db, "unhealthy-node", "10.0.0.8", x86Facts(), false, false)

	facts, ok := GetPersistedNodeHostFacts(context.Background(), "unhealthy-node")
	if !ok || facts == nil || facts.CPUIDHash != "sha256:x86" {
		t.Fatalf("persisted facts must be readable regardless of health, got ok=%v facts=%+v", ok, facts)
	}
	_ = db
}

func TestGetPersistedNodeHostFacts_MissingRow(t *testing.T) {
	newHostFactsQueryDB(t)
	if facts, ok := GetPersistedNodeHostFacts(context.Background(), "nobody"); ok || facts != nil {
		t.Fatalf("missing row must report not found, got ok=%v facts=%+v", ok, facts)
	}
}
