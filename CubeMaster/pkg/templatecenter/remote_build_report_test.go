// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"encoding/json"
	"strings"
	"testing"
)

// mergeRemoteBuildReport is the pure half of preserveRemoteBuildReport: given
// the result_json already on the job and the payload about to replace it, decide
// what should actually be stored. Kept separate from the DB read so the decision
// matrix is testable.
//
// The rule matters because result_json is the ONLY durable evidence that an ext4
// was produced by CubeTemplateCenter rather than in-process, and the finalize
// step writes the template payload to the same column. Losing it means a failed
// remote build cannot be told apart from a failed local one.
func TestMergeRemoteBuildReport(t *testing.T) {
	builtReport := `{"status":"BUILT","artifact_id":"rfs-abc","ext4_sha256":"deadbeef",` +
		`"ext4_size_bytes":1073741824,"image_config_json":"{}"}`
	finalPayload := []byte(`{"template_id":"tpl-abc","status":"FAILED","last_error":"shim timeout"}`)

	tests := []struct {
		name       string
		prior      string
		final      []byte
		wantMerged bool
		wantReason string
	}{
		{
			name:       "built-report-is-preserved",
			prior:      builtReport,
			final:      finalPayload,
			wantMerged: true,
			wantReason: "a remote BUILT report is the evidence this exists to keep",
		},
		{
			name:       "empty-prior-is-a-local-build",
			prior:      "",
			final:      finalPayload,
			wantMerged: false,
			wantReason: "a local build never wrote a BUILT report, so there is nothing to keep",
		},
		{
			name:       "whitespace-prior",
			prior:      "   ",
			final:      finalPayload,
			wantMerged: false,
		},
		{
			name:       "prior-is-already-a-finalize-payload",
			prior:      `{"template_id":"tpl-abc","status":"READY"}`,
			final:      finalPayload,
			wantMerged: false,
			wantReason: "a retry writing over its own output must not nest",
		},
		{
			// The column is written by another code path and could hold anything;
			// unparseable content must never cost us the primary payload.
			name:       "prior-is-not-json",
			prior:      "not json at all",
			final:      finalPayload,
			wantMerged: false,
		},
		{
			name:       "prior-is-a-json-array",
			prior:      `["not","an","object"]`,
			final:      finalPayload,
			wantMerged: false,
		},
		{
			name:       "prior-is-json-null",
			prior:      `null`,
			final:      finalPayload,
			wantMerged: false,
		},
		{
			name:       "lowercase-status-still-counts",
			prior:      `{"status":"built","ext4_sha256":"abc"}`,
			final:      finalPayload,
			wantMerged: true,
		},
		{
			// Never sacrifice the primary payload to keep a diagnostic.
			name:       "unparseable-final-is-returned-untouched",
			prior:      builtReport,
			final:      []byte(`not json`),
			wantMerged: false,
			wantReason: "the payload being stored must survive even if merging fails",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeRemoteBuildReport(tc.prior, tc.final)

			if !tc.wantMerged {
				if string(got) != string(tc.final) {
					t.Fatalf("mergeRemoteBuildReport() = %s, want the final payload unchanged (%s): %s",
						got, tc.final, tc.wantReason)
				}
				return
			}

			var merged map[string]any
			if err := json.Unmarshal(got, &merged); err != nil {
				t.Fatalf("merged result is not valid JSON: %v (%s)", err, got)
			}
			// The finalize payload must survive intact: it is the primary record.
			if merged["template_id"] != "tpl-abc" || merged["status"] != "FAILED" {
				t.Fatalf("merged result lost the final payload: %s", got)
			}
			report, ok := merged[remoteBuildReportKey].(map[string]any)
			if !ok {
				t.Fatalf("merged result does not carry %q: %s", remoteBuildReportKey, got)
			}
			// The ext4 metadata is the part that proves TC measured it.
			if report["ext4_sha256"] == nil {
				t.Fatalf("preserved report lost the ext4 metadata: %s", got)
			}
		})
	}
}

// TestMergeRemoteBuildReportDoesNotNest checks a second finalize (a retry, or a
// redo of the same job) does not wrap a report inside a report, which would grow
// the column without bound.
func TestMergeRemoteBuildReportDoesNotNest(t *testing.T) {
	first := mergeRemoteBuildReport(
		`{"status":"BUILT","ext4_sha256":"abc"}`,
		[]byte(`{"template_id":"tpl-1","status":"FAILED"}`))

	// Feed the merged result back in as the prior value. Its status is FAILED,
	// not BUILT, so nothing should be preserved a second time.
	second := mergeRemoteBuildReport(string(first),
		[]byte(`{"template_id":"tpl-1","status":"FAILED"}`))

	var payload map[string]any
	if err := json.Unmarshal(second, &payload); err != nil {
		t.Fatalf("second merge is not valid JSON: %v", err)
	}
	if _, exists := payload[remoteBuildReportKey]; exists {
		t.Fatalf("a finalize payload must not be preserved as a build report: %s", second)
	}
	if strings.Count(string(second), remoteBuildReportKey) != 0 {
		t.Fatalf("report key leaked into the second merge: %s", second)
	}
}

// TestMergeRemoteBuildReportStripsNestedReport covers the defensive delete: if a
// BUILT report somehow already carries a nested report, only one level is kept.
func TestMergeRemoteBuildReportStripsNestedReport(t *testing.T) {
	prior := `{"status":"BUILT","ext4_sha256":"abc","` + remoteBuildReportKey +
		`":{"status":"BUILT","ext4_sha256":"old"}}`
	got := mergeRemoteBuildReport(prior, []byte(`{"template_id":"tpl-1","status":"READY"}`))

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("merge is not valid JSON: %v", err)
	}
	report, ok := payload[remoteBuildReportKey].(map[string]any)
	if !ok {
		t.Fatalf("expected a preserved report: %s", got)
	}
	if _, nested := report[remoteBuildReportKey]; nested {
		t.Fatalf("only one level of report must be kept: %s", got)
	}
	if report["ext4_sha256"] != "abc" {
		t.Fatalf("the outer report's metadata must win: %s", got)
	}
}
