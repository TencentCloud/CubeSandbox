// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import "testing"

// TestCubeMasterPurgeTables covers the disable_hard_delete × soft_delete_purge
// interaction at the point where it is resolved — the purge table list. The
// purger can only ever touch a table in this list, so asserting membership is
// equivalent to asserting the runtime combination behaviour.
func TestCubeMasterPurgeTables(t *testing.T) {
	defaultTables := cubeMasterPurgeTables(false) // disable_hard_delete=false
	retainTables := cubeMasterPurgeTables(true)   // disable_hard_delete=true

	mustContain := func(tables []string, want string) {
		t.Helper()
		for _, tb := range tables {
			if tb == want {
				return
			}
		}
		t.Errorf("expected %q to be purged, table list = %v", want, tables)
	}
	mustNotContain := func(tables []string, want string) {
		t.Helper()
		for _, tb := range tables {
			if tb == want {
				t.Errorf("%q must NOT be purged (disable_hard_delete retains it), table list = %v", want, tables)
			}
		}
	}

	// Always purged regardless of disable_hard_delete (not instance records).
	for _, tb := range []string{"t_cube_sandbox_spec", "t_cube_template_replica"} {
		mustContain(defaultTables, tb)
		mustContain(retainTables, tb)
	}

	// Default (disable_hard_delete=false): instance tables are purge-eligible.
	// t_cube_instance_info is hard-deleted on the delete path (a defensive
	// no-op for the purger); t_cube_instance_userdata is always soft-deleted
	// and accumulates, so it must be purged.
	mustContain(defaultTables, "t_cube_instance_info")
	mustContain(defaultTables, "t_cube_instance_userdata")

	// Retain mode (disable_hard_delete=true): instance records are EXEMPT.
	// This is the fix for the review finding that the purger bypassed
	// disable_hard_delete and hard-deleted retained instance rows.
	mustNotContain(retainTables, "t_cube_instance_info")
	mustNotContain(retainTables, "t_cube_instance_userdata")
}
