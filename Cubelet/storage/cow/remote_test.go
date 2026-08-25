// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cow

import "testing"

func TestRemoteUUIDsJSONRoundTrip(t *testing.T) {
	t.Parallel()
	u := &RemoteUUIDs{
		Rootfs:   "remote-rootfs",
		Memory:   "remote-memory",
		Metadata: "remote-meta",
	}
	raw := u.JSON()
	got := ParseRemoteUUIDs(raw)
	if got == nil || got.Rootfs != u.Rootfs || got.Memory != u.Memory || got.Metadata != u.Metadata {
		t.Fatalf("roundtrip got %+v from %s", got, raw)
	}
	if ParseRemoteUUIDs("") != nil || ParseRemoteUUIDs("{}") != nil {
		t.Fatal("empty json must parse as nil")
	}
}

func TestExportStatusIsFailed(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"FAILED", "failed", "ERROR", "FAIL"} {
		if !ExportStatusIsFailed(status) {
			t.Fatalf("%q should be a confirmed s3lvol failure", status)
		}
	}
	for _, status := range []string{"", ExportStatusNone, ExportStatusInProgress, ExportStatusDone, "WAT"} {
		if ExportStatusIsFailed(status) {
			t.Fatalf("%q must not be treated as s3lvol failure", status)
		}
	}
}
