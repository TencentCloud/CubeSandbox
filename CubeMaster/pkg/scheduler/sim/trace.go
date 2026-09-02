// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sim replays a request trace against the real scheduler core
// (scheduler.Select) with simulated in-process node state, so scheduling
// quality (packing, fragmentation, balance, herding, template locality) can
// be evaluated at scale without a live cluster.
package sim

import (
	"encoding/json"
	"fmt"
	"os"
)

// Trace file schema — cross-tool contract with cube-bench --dump-trace.
// Field names must stay exactly in sync with examples/cube-bench/sequence.go.

type TraceParams struct {
	RatePerSec   float64 `json:"rate_per_sec"`
	LifetimeMinS float64 `json:"lifetime_min_s"`
	LifetimeMaxS float64 `json:"lifetime_max_s"`
	Total        int     `json:"total"`
}

type TraceTemplate struct {
	TemplateID string `json:"template_id"`
	Weight     int    `json:"weight"`
	CpuMillis  int64  `json:"cpu_millis"`
	MemMiB     int64  `json:"mem_mib"`
}

type TraceRequest struct {
	Seq        int    `json:"seq"`
	ArrivalMs  int64  `json:"arrival_ms"`
	TemplateID string `json:"template_id"`
	CpuMillis  int64  `json:"cpu_millis"`
	MemMiB     int64  `json:"mem_mib"`
	LifetimeMs int64  `json:"lifetime_ms"`
}

type Trace struct {
	Workload    string          `json:"workload"`
	Seed        int64           `json:"seed"`
	GeneratedAt string          `json:"generated_at"`
	Params      TraceParams     `json:"params"`
	Templates   []TraceTemplate `json:"templates"`
	Requests    []TraceRequest  `json:"requests"`
}

// LoadTrace reads and validates a trace file. Requests must be ordered by
// non-decreasing arrival_ms and every request must carry a resource spec:
// cpu_millis/mem_mib of 0 mean the generator did not annotate the shape, which
// the simulator cannot schedule.
func LoadTrace(path string) (*Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	t := &Trace{}
	if err := json.Unmarshal(data, t); err != nil {
		return nil, fmt.Errorf("parse trace %s: %w", path, err)
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("trace %s: %w", path, err)
	}
	return t, nil
}

// validate enforces the trace contract on the in-memory form. LoadTrace calls
// it for file input; RunRound calls it via Params.validate so exported API
// callers get the same guarantees — a malformed request (e.g. negative
// lifetime) would otherwise silently corrupt the round's time-weighted
// metrics instead of producing an error.
func (t *Trace) validate() error {
	if len(t.Requests) == 0 {
		return fmt.Errorf("trace has no requests")
	}
	prev := int64(-1)
	for i := range t.Requests {
		r := &t.Requests[i]
		if r.CpuMillis <= 0 || r.MemMiB <= 0 {
			return fmt.Errorf("request seq=%d has cpu_millis=%d mem_mib=%d: "+
				"requests need a resource spec (cpu_millis/mem_mib > 0); re-generate the trace with spec annotations",
				r.Seq, r.CpuMillis, r.MemMiB)
		}
		if r.ArrivalMs < 0 {
			return fmt.Errorf("request seq=%d has negative arrival_ms=%d", r.Seq, r.ArrivalMs)
		}
		if r.ArrivalMs < prev {
			return fmt.Errorf("requests must be sorted by arrival_ms ascending (seq=%d arrival_ms=%d after %d)",
				r.Seq, r.ArrivalMs, prev)
		}
		if r.LifetimeMs < 0 {
			return fmt.Errorf("request seq=%d has negative lifetime_ms=%d", r.Seq, r.LifetimeMs)
		}
		prev = r.ArrivalMs
	}
	// Cross-check template references: with the shipped strict-locality filter
	// a request whose template_id is absent from the templates section is
	// guaranteed rejection — fail at load, not post-hoc. Only checked when the
	// templates section is present; without it TemplateIDs falls back to
	// request-derived IDs.
	if len(t.Templates) > 0 {
		known := make(map[string]bool, len(t.Templates))
		for _, tpl := range t.Templates {
			known[tpl.TemplateID] = true
		}
		for i := range t.Requests {
			if id := t.Requests[i].TemplateID; id != "" && !known[id] {
				return fmt.Errorf("request seq=%d references template_id %q which is not in the templates section",
					t.Requests[i].Seq, id)
			}
		}
	}
	return nil
}

// MaxRequestCpuMillis returns the largest cpu_millis over all requests — the
// "biggest shape" used by the fragmentation metric.
func (t *Trace) MaxRequestCpuMillis() int64 {
	var m int64
	for i := range t.Requests {
		if t.Requests[i].CpuMillis > m {
			m = t.Requests[i].CpuMillis
		}
	}
	return m
}

// MaxRequestMemMiB returns the largest mem_mib over all requests — the memory
// side of the "biggest shape" used by the fragmentation metric.
func (t *Trace) MaxRequestMemMiB() int64 {
	var m int64
	for i := range t.Requests {
		if t.Requests[i].MemMiB > m {
			m = t.Requests[i].MemMiB
		}
	}
	return m
}

// TemplateIDs returns the distinct non-empty template IDs referenced by the
// templates section, falling back to IDs seen on requests when the templates
// section is absent.
func (t *Trace) TemplateIDs() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, tpl := range t.Templates {
		add(tpl.TemplateID)
	}
	if len(out) == 0 {
		for i := range t.Requests {
			add(t.Requests[i].TemplateID)
		}
	}
	return out
}
