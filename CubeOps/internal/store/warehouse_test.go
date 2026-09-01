// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func TestWarehouseItemQueries(t *testing.T) {
	withWarehouseStores(t, testWarehouseItemQueries)
}

func testWarehouseItemQueries(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()

	missing, err := s.GetWarehouseItem(ctx, "amd64", "cube-shim", "v0.6.0")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("get missing: got %+v, want nil", missing)
	}

	emptyAll, err := s.ListWarehouseItems(ctx)
	if err != nil {
		t.Fatalf("empty list all: %v", err)
	}
	if len(emptyAll) != 0 {
		t.Fatalf("empty list all got %d rows", len(emptyAll))
	}

	emptyShim, err := s.ListWarehouseItemsByComponent(ctx, "cube-shim")
	if err != nil {
		t.Fatalf("empty list shim: %v", err)
	}
	if len(emptyShim) != 0 {
		t.Fatalf("empty warehouse got %d rows", len(emptyShim))
	}

	mustInsert := func(item store.WarehouseItem) {
		t.Helper()
		ok, err := s.InsertWarehouseItem(ctx, item)
		if err != nil {
			t.Fatalf("insert %+v: %v", item, err)
		}
		if !ok {
			t.Fatalf("insert %+v: not inserted", item)
		}
	}
	shimAMD := store.WarehouseItem{
		Arch: "amd64", Component: "cube-shim", Version: "v0.6.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/amd64/cube-shim/v0.6.0/component.tar.gz", SizeBytes: 100, Checksum: "sha256:a",
	}
	mustInsert(shimAMD)
	mustInsert(store.WarehouseItem{
		Arch: "arm64", Component: "cube-shim", Version: "v0.6.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/arm64/cube-shim/v0.6.0/component.tar.gz", SizeBytes: 80, Checksum: "sha256:b",
	})
	mustInsert(store.WarehouseItem{
		Arch: "amd64", Component: "cube-shim", Version: "v0.5.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/amd64/cube-shim/v0.5.0/component.tar.gz", SizeBytes: 90, Checksum: "sha256:c",
	})
	mustInsert(store.WarehouseItem{
		Arch: "amd64", Component: "cube-image", Version: "v0.6.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/amd64/cube-image/v0.6.0/component.tar.gz", SizeBytes: 1, Checksum: "sha256:d",
	})

	got, err := s.GetWarehouseItem(ctx, "amd64", "cube-shim", "v0.6.0")
	if err != nil {
		t.Fatalf("get hit: %v", err)
	}
	if got == nil {
		t.Fatal("get hit: nil")
	}
	if got.Arch != shimAMD.Arch || got.Component != shimAMD.Component || got.Version != shimAMD.Version {
		t.Fatalf("get identity=%s/%s/%s want %s/%s/%s", got.Arch, got.Component, got.Version, shimAMD.Arch, shimAMD.Component, shimAMD.Version)
	}
	if got.Source != shimAMD.Source || got.SourceRef != shimAMD.SourceRef || got.ObjectKey != shimAMD.ObjectKey {
		t.Fatalf("get refs source=%s sourceRef=%s objectKey=%s", got.Source, got.SourceRef, got.ObjectKey)
	}
	if got.SizeBytes != shimAMD.SizeBytes || got.Checksum != shimAMD.Checksum {
		t.Fatalf("get size/checksum=%d %s", got.SizeBytes, got.Checksum)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("get timestamps created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	stillMissing, err := s.GetWarehouseItem(ctx, "amd64", "cube-shim", "no-such")
	if err != nil {
		t.Fatalf("get unknown version: %v", err)
	}
	if stillMissing != nil {
		t.Fatalf("get unknown version: got %+v", stillMissing)
	}

	all, err := s.ListWarehouseItems(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("list all rows=%d want 4", len(all))
	}
	wantOrder := []string{
		"cube-image/v0.6.0/amd64",
		"cube-shim/v0.5.0/amd64",
		"cube-shim/v0.6.0/amd64",
		"cube-shim/v0.6.0/arm64",
	}
	for i, item := range all {
		key := item.Component + "/" + item.Version + "/" + item.Arch
		if key != wantOrder[i] {
			t.Errorf("list all [%d]=%s want %s", i, key, wantOrder[i])
		}
	}

	shim, err := s.ListWarehouseItemsByComponent(ctx, "cube-shim")
	if err != nil {
		t.Fatalf("list shim: %v", err)
	}
	if len(shim) != 3 {
		t.Fatalf("shim rows=%d want 3", len(shim))
	}
	for _, item := range shim {
		if item.Component != "cube-shim" {
			t.Errorf("unexpected component %s", item.Component)
		}
	}
	wantShim := []string{"v0.5.0/amd64", "v0.6.0/amd64", "v0.6.0/arm64"}
	for i, item := range shim {
		key := item.Version + "/" + item.Arch
		if key != wantShim[i] {
			t.Errorf("list shim [%d]=%s want %s", i, key, wantShim[i])
		}
	}

	image, err := s.ListWarehouseItemsByComponent(ctx, "cube-image")
	if err != nil {
		t.Fatalf("list image: %v", err)
	}
	if len(image) != 1 || image[0].Version != "v0.6.0" {
		t.Fatalf("image=%+v", image)
	}

	unknown, err := s.ListWarehouseItemsByComponent(ctx, "cube-kernel-scf")
	if err != nil {
		t.Fatalf("list unknown component: %v", err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown component rows=%d", len(unknown))
	}
}

func TestListImportJobs(t *testing.T) {
	withWarehouseStores(t, testListImportJobs)
}

func testListImportJobs(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()

	empty, total, err := s.ListImportJobs(ctx, 50, 0)
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(empty) != 0 || total != 0 {
		t.Fatalf("empty got %d total=%d", len(empty), total)
	}

	for _, job := range []store.ImportJob{
		{ID: "imp-1", Source: "github", SourceRef: "TencentCloud/CubeSandbox", Tag: "v0.5.0", Arch: "amd64", Status: store.ImportSucceeded},
		{ID: "imp-2", Source: "cnb", SourceRef: "CubeSandbox/CubeSandbox", Tag: "v0.6.0", Arch: "arm64", Status: store.ImportRunning},
		{ID: "imp-3", Source: "upload", SourceRef: "/tmp/x.tar.gz", Tag: "v0.6.1", Arch: "amd64", Status: store.ImportPending},
	} {
		if err := s.CreateImportJob(ctx, job); err != nil {
			t.Fatalf("create %s: %v", job.ID, err)
		}
	}

	all, total, err := s.ListImportJobs(ctx, 50, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("all len=%d total=%d want 3", len(all), total)
	}

	page0, total, err := s.ListImportJobs(ctx, 1, 0)
	if err != nil || total != 3 || len(page0) != 1 {
		t.Fatalf("page0: jobs=%d total=%d err=%v", len(page0), total, err)
	}
	page1, _, err := s.ListImportJobs(ctx, 1, 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("page1: jobs=%d err=%v", len(page1), err)
	}
	if page0[0].ID == page1[0].ID {
		t.Fatalf("pages overlap id=%s", page0[0].ID)
	}

	capped, total, err := s.ListImportJobs(ctx, 0, 0)
	if err != nil || total != 3 || len(capped) != 3 {
		t.Fatalf("default limit: jobs=%d total=%d err=%v", len(capped), total, err)
	}
}

func TestListPreinstallJobs(t *testing.T) {
	withWarehouseStores(t, testListPreinstallJobs)
}

func testListPreinstallJobs(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	mustCreate := func(id, nodeID, status string) {
		t.Helper()
		if err := s.CreatePreinstallJobs(ctx, []store.PreinstallJob{{
			ID: id, NodeID: nodeID, Arch: "amd64", Component: "cube-shim", Version: "v1", Status: status,
		}}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mustCreate("pre-1", "node-a", store.PreinstallSucceeded)
	mustCreate("pre-2", "node-a", store.PreinstallRunning)
	mustCreate("pre-3", "node-b", store.PreinstallPending)

	all, total, err := s.ListPreinstallJobs(ctx, "", "", 50, 0)
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("all: jobs=%d total=%d err=%v", len(all), total, err)
	}
	page0, total, err := s.ListPreinstallJobs(ctx, "", "", 1, 0)
	if err != nil || total != 3 || len(page0) != 1 {
		t.Fatalf("page0: jobs=%d total=%d err=%v", len(page0), total, err)
	}
	page1, _, err := s.ListPreinstallJobs(ctx, "", "", 1, 1)
	if err != nil || len(page1) != 1 || page0[0].ID == page1[0].ID {
		t.Fatalf("page1 overlap or err: %v %v %v", page0, page1, err)
	}
	filtered, total, err := s.ListPreinstallJobs(ctx, "node-a", "", 50, 0)
	if err != nil || total != 2 || len(filtered) != 2 {
		t.Fatalf("node-a: jobs=%d total=%d err=%v", len(filtered), total, err)
	}
	running, total, err := s.ListPreinstallJobs(ctx, "", store.PreinstallRunning, 50, 0)
	if err != nil || total != 1 || len(running) != 1 || running[0].ID != "pre-2" {
		t.Fatalf("running: %+v total=%d err=%v", running, total, err)
	}
}

func TestCreatePreinstallJobsRollsBackOnConflict(t *testing.T) {
	withWarehouseStores(t, testCreatePreinstallJobsRollsBackOnConflict)
}

func testCreatePreinstallJobsRollsBackOnConflict(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	err := s.CreatePreinstallJobs(ctx, []store.PreinstallJob{
		{ID: "pre-dup", NodeID: "node-a", Arch: "amd64", Component: "cube-shim", Version: "v1", Status: store.PreinstallPending},
		{ID: "pre-dup", NodeID: "node-b", Arch: "amd64", Component: "cube-shim", Version: "v1", Status: store.PreinstallPending},
	})
	if err == nil {
		t.Fatal("expected duplicate-id error")
	}
	jobs, total, listErr := s.ListPreinstallJobs(ctx, "", "", 50, 0)
	if listErr != nil || total != 0 || len(jobs) != 0 {
		t.Fatalf("partial insert leaked: jobs=%d total=%d err=%v", len(jobs), total, listErr)
	}
}

func TestClaimImportJob(t *testing.T) {
	withWarehouseStores(t, testClaimImportJob)
}

func testClaimImportJob(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	stale := 30 * time.Minute

	mustCreate := func(id, status string) {
		t.Helper()
		err := s.CreateImportJob(ctx, store.ImportJob{
			ID: id, Source: "upload", SourceRef: "/tmp/x.tar.gz",
			Tag: "v0.6.0", Arch: "amd64", Status: status,
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	mustCreate("claim-pending", store.ImportPending)
	work, err := s.ListImportWork(ctx, stale)
	if err != nil {
		t.Fatalf("list work: %v", err)
	}
	if len(work) != 1 || work[0].ID != "claim-pending" {
		t.Fatalf("list pending work=%v", work)
	}

	ok, err := s.ClaimImportJob(ctx, "claim-pending", stale)
	if err != nil || !ok {
		t.Fatalf("claim pending: ok=%v err=%v", ok, err)
	}
	got, err := s.GetImportJob(ctx, "claim-pending")
	if err != nil {
		t.Fatalf("get after claim: %v", err)
	}
	if got == nil || got.Status != store.ImportRunning {
		t.Fatalf("after claim status=%v", got)
	}

	ok, err = s.ClaimImportJob(ctx, "claim-pending", stale)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("second claim of running job succeeded")
	}

	mustCreate("claim-fresh-running", store.ImportRunning)
	ok, err = s.ClaimImportJob(ctx, "claim-fresh-running", stale)
	if err != nil {
		t.Fatalf("fresh running claim: %v", err)
	}
	if ok {
		t.Fatal("fresh running job was reclaimed")
	}

	mustCreate("claim-stale-running", store.ImportRunning)
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_component_import_job SET updated_at = ? WHERE id = ?`,
		past, "claim-stale-running",
	).Error; err != nil {
		t.Fatalf("backdate stale running: %v", err)
	}
	work, err = s.ListImportWork(ctx, stale)
	if err != nil {
		t.Fatalf("list stale work: %v", err)
	}
	foundStale := false
	for _, job := range work {
		if job.ID == "claim-stale-running" {
			foundStale = true
		}
		if job.ID == "claim-fresh-running" || job.ID == "claim-pending" {
			t.Fatalf("list work included non-stale %s", job.ID)
		}
	}
	if !foundStale {
		t.Fatal("list work missing stale running job")
	}

	ok, err = s.ClaimImportJob(ctx, "claim-stale-running", stale)
	if err != nil || !ok {
		t.Fatalf("reclaim stale running: ok=%v err=%v", ok, err)
	}
}

func TestReplaceNodeInstalls(t *testing.T) {
	withWarehouseStores(t, testReplaceNodeInstalls)
}

func testReplaceNodeInstalls(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	node := "10.10.0.8"
	other := "10.10.0.9"
	arch := "amd64"

	mustReplace := func(nodeID string, items []store.NodeInstall) {
		t.Helper()
		if err := s.ReplaceNodeInstalls(ctx, nodeID, arch, items); err != nil {
			t.Fatalf("replace %s: %v", nodeID, err)
		}
	}
	keysOf := func(nodeID string) map[string]int64 {
		t.Helper()
		rows, err := s.DB().WithContext(ctx).Raw(
			`SELECT id, component, version FROM t_component_node_install WHERE node_id = ? AND arch = ?`,
			nodeID, arch,
		).Rows()
		if err != nil {
			t.Fatalf("select ids: %v", err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var id int64
			var component, version string
			if err := rows.Scan(&id, &component, &version); err != nil {
				t.Fatalf("scan id: %v", err)
			}
			out[component+"/"+version] = id
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("ids rows: %v", err)
		}
		return out
	}

	mustReplace(node, []store.NodeInstall{
		{Component: "cube-shim", Version: "vA"},
		{Component: "cube-agent", Version: "vB"},
	})
	mustReplace(other, []store.NodeInstall{
		{Component: "cube-shim", Version: "vKeep"},
	})

	got, err := s.ListNodeInstalls(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasInstall(got, node, "cube-shim", "vA") || !hasInstall(got, node, "cube-agent", "vB") {
		t.Fatalf("missing A/B after first replace: %+v", got)
	}

	before := keysOf(node)
	mustReplace(node, []store.NodeInstall{
		{Component: "cube-agent", Version: "vB"},
		{Component: "cube-shim", Version: "vA"},
	})
	after := keysOf(node)
	if len(after) != 2 {
		t.Fatalf("same-set rows=%d want 2", len(after))
	}
	for k, id := range before {
		if after[k] != id {
			t.Fatalf("same-set rewrote %s id %d -> %d", k, id, after[k])
		}
	}

	mustReplace(node, []store.NodeInstall{
		{Component: "cube-shim", Version: "vA"},
	})
	got, err = s.ListNodeInstalls(ctx)
	if err != nil {
		t.Fatalf("list after shrink: %v", err)
	}
	if !hasInstall(got, node, "cube-shim", "vA") {
		t.Fatal("missing A after shrink")
	}
	if hasInstall(got, node, "cube-agent", "vB") {
		t.Fatal("B still present after shrink")
	}
	if !hasInstall(got, other, "cube-shim", "vKeep") {
		t.Fatal("other node row was cleared")
	}

	mustReplace(node, nil)
	got, err = s.ListNodeInstalls(ctx)
	if err != nil {
		t.Fatalf("list after empty: %v", err)
	}
	for _, inst := range got {
		if inst.NodeID == node {
			t.Fatalf("empty replace left %+v", inst)
		}
	}
	if !hasInstall(got, other, "cube-shim", "vKeep") {
		t.Fatal("other node row missing after empty replace")
	}
}

func hasInstall(items []store.NodeInstall, nodeID, component, version string) bool {
	for _, inst := range items {
		if inst.NodeID == nodeID && inst.Component == component && inst.Version == version {
			return true
		}
	}
	return false
}

func TestListNodePreinstallWorkStaleRunning(t *testing.T) {
	withWarehouseStores(t, testListNodePreinstallWorkStaleRunning)
}

func testListNodePreinstallWorkStaleRunning(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	stale := 15 * time.Minute
	mustCreate := func(id, nodeID, status string) {
		t.Helper()
		if err := s.CreatePreinstallJobs(ctx, []store.PreinstallJob{{
			ID: id, NodeID: nodeID, Arch: "amd64", Component: "cube-shim", Version: "v1", Status: status,
		}}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mustCreate("pre-pending", "node-a", store.PreinstallPending)
	mustCreate("pre-fresh", "node-a", store.PreinstallRunning)
	mustCreate("pre-stale", "node-a", store.PreinstallRunning)
	mustCreate("pre-other", "node-b", store.PreinstallPending)

	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_component_preinstall_job SET updated_at = ? WHERE id = ?`,
		past, "pre-stale",
	).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}

	work, err := s.ListNodePreinstallWork(ctx, "node-a", stale)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := map[string]bool{}
	for _, job := range work {
		found[job.ID] = true
	}
	if !found["pre-pending"] || !found["pre-stale"] {
		t.Fatalf("work=%v want pending+stale", found)
	}
	if found["pre-fresh"] || found["pre-other"] {
		t.Fatalf("work included non-stale or other node: %v", found)
	}
}

func TestCancelPendingPreinstallForVersionIncludesRunning(t *testing.T) {
	withWarehouseStores(t, testCancelPendingPreinstallForVersionIncludesRunning)
}

func testCancelPendingPreinstallForVersionIncludesRunning(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	mustCreate := func(id, status, version string) {
		t.Helper()
		if err := s.CreatePreinstallJobs(ctx, []store.PreinstallJob{{
			ID: id, NodeID: "node-a", Arch: "amd64", Component: "cube-shim", Version: version, Status: status,
		}}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mustCreate("c-pending", store.PreinstallPending, "v1")
	mustCreate("c-running", store.PreinstallRunning, "v1")
	mustCreate("c-failed", store.PreinstallFailed, "v1")
	mustCreate("c-ok", store.PreinstallSucceeded, "v1")
	mustCreate("c-other", store.PreinstallRunning, "v2")

	if err := s.CancelPendingPreinstallForVersion(ctx, "amd64", "cube-shim", "v1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	jobs, _, err := s.ListPreinstallJobs(ctx, "", "", 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	statusOf := map[string]string{}
	for _, job := range jobs {
		statusOf[job.ID] = job.Status
	}
	for _, id := range []string{"c-pending", "c-running", "c-failed"} {
		if statusOf[id] != store.PreinstallCancelled {
			t.Errorf("%s status=%s want cancelled", id, statusOf[id])
		}
	}
	if statusOf["c-ok"] != store.PreinstallSucceeded {
		t.Errorf("succeeded job was cancelled")
	}
	if statusOf["c-other"] != store.PreinstallRunning {
		t.Errorf("other version running job status=%s", statusOf["c-other"])
	}
}

func TestTouchImportJob(t *testing.T) {
	withWarehouseStores(t, testTouchImportJob)
}

func testTouchImportJob(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateImportJob(ctx, store.ImportJob{
		ID: "touch-1", Source: "upload", SourceRef: "uploads/x.tar.gz",
		Tag: "v0.6.0", Arch: "amd64", Status: store.ImportRunning,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetImportJob(ctx, "touch-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %+v", err, got)
	}
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_component_import_job SET updated_at = ? WHERE id = ?`,
		past, "touch-1",
	).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}
	ok, err := s.TouchImportJob(ctx, "touch-1")
	if err != nil || !ok {
		t.Fatalf("touch running: ok=%v err=%v", ok, err)
	}
	got, err = s.GetImportJob(ctx, "touch-1")
	if err != nil || got == nil {
		t.Fatalf("get after touch: %v %+v", err, got)
	}
	if !got.UpdatedAt.After(past) {
		t.Fatalf("updated_at did not advance: after=%v", got.UpdatedAt)
	}
	if err := s.UpdateImportJob(ctx, "touch-1", store.ImportSucceeded, "", 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	ok, err = s.TouchImportJob(ctx, "touch-1")
	if err != nil {
		t.Fatalf("touch finished: %v", err)
	}
	if ok {
		t.Fatal("touch of non-running job returned true")
	}
}

func TestCountLiveImportJobsBySourceRef(t *testing.T) {
	withWarehouseStores(t, testCountLiveImportJobsBySourceRef)
}

func testCountLiveImportJobsBySourceRef(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	ref := "uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.tar.gz"
	mustCreate := func(id, status string) {
		t.Helper()
		if err := s.CreateImportJob(ctx, store.ImportJob{
			ID: id, Source: "upload", SourceRef: ref, Tag: "v0.6.0", Arch: "amd64", Status: status,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mustCreate("live-pending", store.ImportPending)
	mustCreate("live-running", store.ImportRunning)
	mustCreate("done", store.ImportSucceeded)
	if err := s.CreateImportJob(ctx, store.ImportJob{
		ID: "other-ref", Source: "upload", SourceRef: "uploads/other.tar.gz",
		Tag: "v0.6.0", Arch: "arm64", Status: store.ImportPending,
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}

	n, err := s.CountLiveImportJobsBySourceRef(ctx, ref)
	if err != nil || n != 2 {
		t.Fatalf("live=%d err=%v want 2", n, err)
	}
	n, err = s.CountLiveImportJobsBySourceRef(ctx, "/tmp/missing.tar.gz")
	if err != nil || n != 0 {
		t.Fatalf("missing=%d err=%v want 0", n, err)
	}
}
