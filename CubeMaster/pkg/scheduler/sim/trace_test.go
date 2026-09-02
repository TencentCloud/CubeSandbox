// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTrace(t *testing.T, tr *Trace) string {
	t.Helper()
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validTrace() *Trace {
	return &Trace{
		Workload: "burst",
		Seed:     42,
		Templates: []TraceTemplate{
			{TemplateID: "tpl-a", Weight: 1, CpuMillis: 1000, MemMiB: 2048},
		},
		Requests: []TraceRequest{
			{Seq: 0, ArrivalMs: 0, TemplateID: "tpl-a", CpuMillis: 1000, MemMiB: 2048, LifetimeMs: 1000},
			{Seq: 1, ArrivalMs: 500, TemplateID: "tpl-a", CpuMillis: 2000, MemMiB: 4096, LifetimeMs: 1000},
		},
	}
}

func TestLoadTraceOK(t *testing.T) {
	tr, err := LoadTrace(writeTrace(t, validTrace()))
	if err != nil {
		t.Fatalf("LoadTrace: %v", err)
	}
	if len(tr.Requests) != 2 || tr.Requests[1].CpuMillis != 2000 {
		t.Fatalf("unexpected trace: %+v", tr)
	}
	if got := tr.MaxRequestCpuMillis(); got != 2000 {
		t.Fatalf("MaxRequestCpuMillis=%d, want 2000", got)
	}
	if ids := tr.TemplateIDs(); len(ids) != 1 || ids[0] != "tpl-a" {
		t.Fatalf("TemplateIDs=%v", ids)
	}
}

func TestLoadTraceRejectsZeroSpec(t *testing.T) {
	tr := validTrace()
	tr.Requests[1].CpuMillis = 0
	_, err := LoadTrace(writeTrace(t, tr))
	if err == nil || !strings.Contains(err.Error(), "resource spec") {
		t.Fatalf("want resource-spec error, got %v", err)
	}

	tr = validTrace()
	tr.Requests[0].MemMiB = 0
	if _, err := LoadTrace(writeTrace(t, tr)); err == nil {
		t.Fatal("want error for mem_mib=0")
	}
}

func TestLoadTraceRejectsUnsorted(t *testing.T) {
	tr := validTrace()
	tr.Requests[1].ArrivalMs = 499
	tr.Requests[0].ArrivalMs = 500
	_, err := LoadTrace(writeTrace(t, tr))
	if err == nil || !strings.Contains(err.Error(), "arrival_ms") {
		t.Fatalf("want arrival order error, got %v", err)
	}
}

func TestLoadTraceRejectsNegativeLifetime(t *testing.T) {
	tr := validTrace()
	tr.Requests[0].LifetimeMs = -1
	if _, err := LoadTrace(writeTrace(t, tr)); err == nil {
		t.Fatal("want error for negative lifetime")
	}
}

func TestLoadTraceRejectsOverflowMagnitudes(t *testing.T) {
	// The engine computes arrival_ms+lifetime_ms and mem_mib*1024*1024 in
	// int64; absurd magnitudes must fail at load instead of wrapping.
	tr := validTrace()
	tr.Requests[1].LifetimeMs = math.MaxInt64
	if _, err := LoadTrace(writeTrace(t, tr)); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("want arrival+lifetime overflow error, got %v", err)
	}

	tr = validTrace()
	tr.Requests[0].MemMiB = math.MaxInt64/(1024*1024) + 1
	if _, err := LoadTrace(writeTrace(t, tr)); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("want mem_mib byte-conversion overflow error, got %v", err)
	}
}

func TestTemplateIDsFallbackToRequests(t *testing.T) {
	tr := validTrace()
	tr.Templates = nil
	got, err := LoadTrace(writeTrace(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	if ids := got.TemplateIDs(); len(ids) != 1 || ids[0] != "tpl-a" {
		t.Fatalf("TemplateIDs=%v", ids)
	}
}

func TestLoadTraceRejectsUnknownTemplate(t *testing.T) {
	// With the templates section present, a request referencing an undeclared
	// template_id is guaranteed rejection under strict locality — fail at
	// load instead of producing an all-rejected round.
	tr := validTrace()
	tr.Requests[1].TemplateID = "tpl-ghost"
	_, err := LoadTrace(writeTrace(t, tr))
	if err == nil || !strings.Contains(err.Error(), "tpl-ghost") {
		t.Fatalf("want unknown-template error, got %v", err)
	}

	// Without a templates section the check is skipped (TemplateIDs falls
	// back to request-derived IDs).
	tr = validTrace()
	tr.Templates = nil
	tr.Requests[1].TemplateID = "tpl-other"
	if _, err := LoadTrace(writeTrace(t, tr)); err != nil {
		t.Fatalf("trace without templates section must stay loadable: %v", err)
	}
}
