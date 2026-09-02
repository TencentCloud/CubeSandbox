package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

// TemplateSpec describes one template pool entry for scheduled workloads.
// CpuMillis/MemMiB are spec annotations carried into the trace file; they are
// not sent to the API (resource sizing is a template-side concern).
type TemplateSpec struct {
	TemplateID string
	Weight     int
	CpuMillis  int64
	MemMiB     int64
}

// ScheduledRequest is one pre-generated request in a workload sequence.
type ScheduledRequest struct {
	Seq           int
	ArrivalOffset time.Duration // offset from benchmark start
	TemplateID    string
	CpuMillis     int64
	MemMiB        int64
	Lifetime      time.Duration // 0 = delete immediately after create
}

// workloadPreset holds flag defaults for a named workload. Explicitly passed
// flags always win over preset values (see applyWorkloadPreset).
type workloadPreset struct {
	total   int
	rate    float64
	lifeMin float64 // seconds
	lifeMax float64 // seconds
}

var workloadPresets = map[string]workloadPreset{
	"burst":          {total: 500, rate: 50, lifeMin: 10, lifeMax: 120},
	"template_storm": {total: 300, rate: 30, lifeMin: 30, lifeMax: 90},
	"mixed_spec":     {total: 400, rate: 10, lifeMin: 30, lifeMax: 300},
}

// applyWorkloadPreset fills cfg with preset defaults for flags the user did
// not pass explicitly. explicit holds flag names seen by flag.Visit.
func applyWorkloadPreset(cfg *Config, explicit map[string]bool) error {
	p, ok := workloadPresets[cfg.Workload]
	if !ok {
		return fmt.Errorf("unknown workload %q (valid: burst, template_storm, mixed_spec)", cfg.Workload)
	}
	if !explicit["n"] && !explicit["total"] {
		cfg.Total = p.total
	}
	if !explicit["rate"] {
		cfg.Rate = p.rate
	}
	if !explicit["lifetime"] {
		cfg.LifetimeMin, cfg.LifetimeMax, cfg.hasLifetime = p.lifeMin, p.lifeMax, true
	}
	return nil
}

// parseTemplates parses "id[:weight[:cpuMillis:memMiB]],..." entries.
// Weight defaults to 1, cpu/mem default to 0 (unknown spec).
func parseTemplates(spec string) ([]TemplateSpec, error) {
	var out []TemplateSpec
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("--templates contains an empty entry")
		}
		fields := strings.Split(part, ":")
		if len(fields) > 4 {
			return nil, fmt.Errorf("--templates entry %q: want templateID[:weight[:cpuMillis:memMiB]]", part)
		}
		ts := TemplateSpec{TemplateID: strings.TrimSpace(fields[0]), Weight: 1}
		if ts.TemplateID == "" {
			return nil, fmt.Errorf("--templates entry %q: template ID must not be empty", part)
		}
		if len(fields) >= 2 {
			w, err := strconv.Atoi(strings.TrimSpace(fields[1]))
			if err != nil || w < 1 {
				return nil, fmt.Errorf("--templates entry %q: weight must be a positive integer", part)
			}
			ts.Weight = w
		}
		if len(fields) >= 3 {
			cpu, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
			if err != nil || cpu < 0 {
				return nil, fmt.Errorf("--templates entry %q: cpuMillis must be a non-negative integer", part)
			}
			ts.CpuMillis = cpu
		}
		if len(fields) >= 4 {
			mem, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
			if err != nil || mem < 0 {
				return nil, fmt.Errorf("--templates entry %q: memMiB must be a non-negative integer", part)
			}
			ts.MemMiB = mem
		}
		out = append(out, ts)
	}
	return out, nil
}

// parseLifetime parses "min,max" (seconds). A single value means a constant
// lifetime (min == max).
func parseLifetime(spec string) (float64, float64, error) {
	parts := strings.Split(spec, ",")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("--lifetime must be min,max in seconds (got %q)", spec)
	}
	lo, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil || lo < 0 {
		return 0, 0, fmt.Errorf("--lifetime min must be a non-negative number (got %q)", spec)
	}
	hi := lo
	if len(parts) == 2 {
		hi, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || hi < 0 {
			return 0, 0, fmt.Errorf("--lifetime max must be a non-negative number (got %q)", spec)
		}
	}
	if hi < lo {
		return 0, 0, fmt.Errorf("--lifetime max (%.3g) must be >= min (%.3g)", hi, lo)
	}
	return lo, hi, nil
}

// GenerateSequence pre-generates the full request schedule with a dedicated
// rng so identical seeds reproduce identical sequences. Draw order per request
// is fixed: arrival interval, lifetime, template pick.
func GenerateSequence(cfg *Config, rng *rand.Rand) []ScheduledRequest {
	seq := make([]ScheduledRequest, cfg.Total)

	totalWeight := 0
	for _, t := range cfg.Templates {
		totalWeight += t.Weight
	}

	var offset time.Duration
	for i := range seq {
		sr := ScheduledRequest{Seq: i, ArrivalOffset: offset}
		// Poisson process: inter-arrival times are Exp(λ). The first request
		// goes out immediately (offset 0). rate <= 0 means "as fast as
		// possible", i.e. every offset stays 0 like the legacy flood.
		if cfg.Rate > 0 {
			offset += time.Duration(rng.ExpFloat64() / cfg.Rate * float64(time.Second))
		}
		if cfg.hasLifetime {
			lt := cfg.LifetimeMin + rng.Float64()*(cfg.LifetimeMax-cfg.LifetimeMin)
			sr.Lifetime = time.Duration(lt * float64(time.Second))
		}
		if totalWeight > 0 {
			t := pickTemplate(cfg.Templates, totalWeight, rng)
			sr.TemplateID = t.TemplateID
			sr.CpuMillis = t.CpuMillis
			sr.MemMiB = t.MemMiB
		}
		seq[i] = sr
	}
	return seq
}

func pickTemplate(templates []TemplateSpec, totalWeight int, rng *rand.Rand) TemplateSpec {
	draw := rng.Intn(totalWeight)
	for _, t := range templates {
		if draw < t.Weight {
			return t
		}
		draw -= t.Weight
	}
	return templates[len(templates)-1]
}

// workloadDisplayName is the label used in traces/reports; ad-hoc scheduled
// runs (new flags without --workload) are reported as "custom".
func workloadDisplayName(cfg *Config) string {
	if cfg.Workload != "" {
		return cfg.Workload
	}
	return "custom"
}

// ── Trace file (cross-tool contract; field names must stay stable) ──

type traceParams struct {
	RatePerSec   float64 `json:"rate_per_sec"`
	LifetimeMinS float64 `json:"lifetime_min_s"`
	LifetimeMaxS float64 `json:"lifetime_max_s"`
	Total        int     `json:"total"`
}

type traceTemplate struct {
	TemplateID string `json:"template_id"`
	Weight     int    `json:"weight"`
	CpuMillis  int64  `json:"cpu_millis"`
	MemMiB     int64  `json:"mem_mib"`
}

type traceRequest struct {
	Seq        int    `json:"seq"`
	ArrivalMs  int64  `json:"arrival_ms"`
	TemplateID string `json:"template_id"`
	CpuMillis  int64  `json:"cpu_millis"`
	MemMiB     int64  `json:"mem_mib"`
	LifetimeMs int64  `json:"lifetime_ms"`
}

type traceFile struct {
	Workload    string          `json:"workload"`
	Seed        int64           `json:"seed"`
	GeneratedAt string          `json:"generated_at"`
	Params      traceParams     `json:"params"`
	Templates   []traceTemplate `json:"templates"`
	Requests    []traceRequest  `json:"requests"`
}

// DumpTrace writes the pre-generated sequence as a JSON trace file that other
// tooling (e.g. a scheduling simulator) can replay. arrival_ms/lifetime_ms
// are integer milliseconds (Duration.Milliseconds(), truncated toward zero),
// while the JSON export's raw entries keep float-millisecond precision for
// the same seq — cross-checking the two files can show sub-millisecond
// differences, which is expected.
func DumpTrace(path string, cfg *Config, seq []ScheduledRequest) error {
	tf := traceFile{
		Workload:    workloadDisplayName(cfg),
		Seed:        cfg.Seed,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Params: traceParams{
			RatePerSec:   cfg.Rate,
			LifetimeMinS: cfg.LifetimeMin,
			LifetimeMaxS: cfg.LifetimeMax,
			Total:        cfg.Total,
		},
	}
	tf.Templates = make([]traceTemplate, len(cfg.Templates))
	for i, t := range cfg.Templates {
		tf.Templates[i] = traceTemplate{
			TemplateID: t.TemplateID,
			Weight:     t.Weight,
			CpuMillis:  t.CpuMillis,
			MemMiB:     t.MemMiB,
		}
	}
	tf.Requests = make([]traceRequest, len(seq))
	for i, sr := range seq {
		tf.Requests[i] = traceRequest{
			Seq:        sr.Seq,
			ArrivalMs:  sr.ArrivalOffset.Milliseconds(),
			TemplateID: sr.TemplateID,
			CpuMillis:  sr.CpuMillis,
			MemMiB:     sr.MemMiB,
			LifetimeMs: sr.Lifetime.Milliseconds(),
		}
	}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("trace marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("trace write: %w", err)
	}
	return nil
}
