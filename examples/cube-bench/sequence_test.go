package main

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func schedTestConfig() *Config {
	return &Config{
		Total:       100,
		Rate:        50,
		hasLifetime: true,
		LifetimeMin: 10,
		LifetimeMax: 120,
		Templates:   []TemplateSpec{{TemplateID: "tpl-a", Weight: 1}},
	}
}

func TestGenerateSequenceDeterministic(t *testing.T) {
	cfg := schedTestConfig()
	cfg.Templates = []TemplateSpec{
		{TemplateID: "tpl-a", Weight: 6, CpuMillis: 1000, MemMiB: 2048},
		{TemplateID: "tpl-b", Weight: 3, CpuMillis: 2000, MemMiB: 4096},
		{TemplateID: "tpl-c", Weight: 1, CpuMillis: 8000, MemMiB: 16384},
	}

	a := GenerateSequence(cfg, rand.New(rand.NewSource(42)))
	b := GenerateSequence(cfg, rand.New(rand.NewSource(42)))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different sequences")
	}

	c := GenerateSequence(cfg, rand.New(rand.NewSource(43)))
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical sequences")
	}
}

func TestGenerateSequenceInterArrivalMean(t *testing.T) {
	cfg := schedTestConfig()
	cfg.Total = 1500
	cfg.Rate = 50 // expected mean inter-arrival = 20ms

	seq := GenerateSequence(cfg, rand.New(rand.NewSource(7)))
	if seq[0].ArrivalOffset != 0 {
		t.Fatalf("first arrival offset = %v, want 0", seq[0].ArrivalOffset)
	}

	var sum time.Duration
	for i := 1; i < len(seq); i++ {
		d := seq[i].ArrivalOffset - seq[i-1].ArrivalOffset
		if d < 0 {
			t.Fatalf("arrival offsets not ascending at %d", i)
		}
		sum += d
	}
	mean := float64(sum) / float64(len(seq)-1) / float64(time.Millisecond)
	want := 1000.0 / cfg.Rate // 20ms
	if math.Abs(mean-want) > want*0.2 {
		t.Fatalf("mean inter-arrival = %.2fms, want %.2fms ±20%%", mean, want)
	}
}

func TestGenerateSequenceZeroRateFloods(t *testing.T) {
	cfg := schedTestConfig()
	cfg.Rate = 0
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(1)))
	for i, sr := range seq {
		if sr.ArrivalOffset != 0 {
			t.Fatalf("seq[%d].ArrivalOffset = %v, want 0 for rate=0", i, sr.ArrivalOffset)
		}
	}
}

func TestGenerateSequenceLifetimeBounds(t *testing.T) {
	cfg := schedTestConfig()
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(9)))
	lo := time.Duration(cfg.LifetimeMin * float64(time.Second))
	hi := time.Duration(cfg.LifetimeMax * float64(time.Second))
	for i, sr := range seq {
		if sr.Lifetime < lo || sr.Lifetime > hi {
			t.Fatalf("seq[%d].Lifetime = %v, want within [%v, %v]", i, sr.Lifetime, lo, hi)
		}
	}
}

func TestGenerateSequenceNoLifetime(t *testing.T) {
	cfg := schedTestConfig()
	cfg.hasLifetime = false
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(9)))
	for i, sr := range seq {
		if sr.Lifetime != 0 {
			t.Fatalf("seq[%d].Lifetime = %v, want 0 without --lifetime", i, sr.Lifetime)
		}
	}
}

func TestGenerateSequenceTemplateWeights(t *testing.T) {
	cfg := schedTestConfig()
	cfg.Total = 6000
	cfg.Templates = []TemplateSpec{
		{TemplateID: "small", Weight: 6},
		{TemplateID: "medium", Weight: 3},
		{TemplateID: "large", Weight: 1},
	}
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(11)))

	counts := map[string]int{}
	for _, sr := range seq {
		counts[sr.TemplateID]++
		if sr.TemplateID == "" {
			t.Fatalf("seq[%d] has empty template", sr.Seq)
		}
	}
	want := map[string]float64{"small": 0.6, "medium": 0.3, "large": 0.1}
	for id, p := range want {
		got := float64(counts[id]) / float64(cfg.Total)
		if math.Abs(got-p) > p*0.35 {
			t.Fatalf("template %s share = %.3f, want %.3f (loose)", id, got, p)
		}
	}
}

func TestGenerateSequenceCarriesSpecAnnotations(t *testing.T) {
	cfg := schedTestConfig()
	cfg.Total = 10
	cfg.Templates = []TemplateSpec{{TemplateID: "tpl-a", Weight: 1, CpuMillis: 2000, MemMiB: 4096}}
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(3)))
	for i, sr := range seq {
		if sr.CpuMillis != 2000 || sr.MemMiB != 4096 {
			t.Fatalf("seq[%d] spec = %d/%d, want 2000/4096", i, sr.CpuMillis, sr.MemMiB)
		}
	}
}

func TestParseTemplates(t *testing.T) {
	got, err := parseTemplates("tpl-a, tpl-b:2, tpl-c:3:1000:2048")
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	want := []TemplateSpec{
		{TemplateID: "tpl-a", Weight: 1},
		{TemplateID: "tpl-b", Weight: 2},
		{TemplateID: "tpl-c", Weight: 3, CpuMillis: 1000, MemMiB: 2048},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseTemplatesRejectsBadSpecs(t *testing.T) {
	for _, spec := range []string{
		"",
		"tpl-a,,tpl-b",
		":2",
		"tpl-a:0",
		"tpl-a:-1",
		"tpl-a:x",
		"tpl-a:1:-5",
		"tpl-a:1:1000:-1",
		"tpl-a:1:1000:2048:extra",
	} {
		if _, err := parseTemplates(spec); err == nil {
			t.Fatalf("parseTemplates(%q) returned nil error, want rejection", spec)
		}
	}
}

func TestParseLifetime(t *testing.T) {
	lo, hi, err := parseLifetime("10,120")
	if err != nil || lo != 10 || hi != 120 {
		t.Fatalf("parseLifetime(10,120) = %v,%v,%v", lo, hi, err)
	}
	lo, hi, err = parseLifetime("30")
	if err != nil || lo != 30 || hi != 30 {
		t.Fatalf("parseLifetime(30) = %v,%v,%v", lo, hi, err)
	}
	for _, spec := range []string{"", "abc", "120,10", "-5,10", "1,2,3"} {
		if _, _, err := parseLifetime(spec); err == nil {
			t.Fatalf("parseLifetime(%q) returned nil error, want rejection", spec)
		}
	}
}

func TestApplyWorkloadPreset(t *testing.T) {
	cfg := &Config{Workload: "burst", Total: 20}
	if err := applyWorkloadPreset(cfg, map[string]bool{}); err != nil {
		t.Fatalf("applyWorkloadPreset: %v", err)
	}
	if cfg.Total != 500 || cfg.Rate != 50 || cfg.LifetimeMin != 10 || cfg.LifetimeMax != 120 || !cfg.hasLifetime {
		t.Fatalf("burst preset not applied: %+v", cfg)
	}

	// Explicit flags win over preset defaults.
	cfg = &Config{Workload: "burst", Total: 20, Rate: 5}
	explicit := map[string]bool{"n": true, "rate": true}
	if err := applyWorkloadPreset(cfg, explicit); err != nil {
		t.Fatalf("applyWorkloadPreset: %v", err)
	}
	if cfg.Total != 20 || cfg.Rate != 5 {
		t.Fatalf("explicit flags overridden: total=%d rate=%v", cfg.Total, cfg.Rate)
	}
	if !cfg.hasLifetime || cfg.LifetimeMin != 10 || cfg.LifetimeMax != 120 {
		t.Fatalf("lifetime preset not applied: %+v", cfg)
	}

	cfg = &Config{Workload: "mixed_spec", Total: 20}
	if err := applyWorkloadPreset(cfg, map[string]bool{}); err != nil {
		t.Fatalf("applyWorkloadPreset: %v", err)
	}
	if cfg.Total != 400 || cfg.Rate != 10 || cfg.LifetimeMin != 30 || cfg.LifetimeMax != 300 {
		t.Fatalf("mixed_spec preset not applied: %+v", cfg)
	}

	if err := applyWorkloadPreset(&Config{Workload: "nope"}, map[string]bool{}); err == nil {
		t.Fatal("unknown workload returned nil error")
	}
}

func TestDumpTraceSchema(t *testing.T) {
	cfg := &Config{
		Workload:    "burst",
		Seed:        7,
		Total:       50,
		Rate:        50,
		hasLifetime: true,
		LifetimeMin: 10,
		LifetimeMax: 120,
		Templates: []TemplateSpec{
			{TemplateID: "tpl-small", Weight: 1, CpuMillis: 1000, MemMiB: 2048},
		},
	}
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(cfg.Seed)))

	path := filepath.Join(t.TempDir(), "trace.json")
	if err := DumpTrace(path, cfg, seq); err != nil {
		t.Fatalf("DumpTrace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("trace unmarshal: %v", err)
	}
	for _, key := range []string{"workload", "seed", "generated_at", "params", "templates", "requests"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("trace missing top-level key %q", key)
		}
	}
	if string(top["workload"]) != `"burst"` {
		t.Fatalf("workload = %s, want \"burst\"", top["workload"])
	}
	if string(top["seed"]) != "7" {
		t.Fatalf("seed = %s, want 7", top["seed"])
	}
	var genAt string
	if err := json.Unmarshal(top["generated_at"], &genAt); err != nil {
		t.Fatalf("generated_at unmarshal: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, genAt); err != nil {
		t.Fatalf("generated_at %q not RFC3339: %v", genAt, err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(top["params"], &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	for _, key := range []string{"rate_per_sec", "lifetime_min_s", "lifetime_max_s", "total"} {
		if _, ok := params[key]; !ok {
			t.Fatalf("params missing key %q", key)
		}
	}
	if string(params["rate_per_sec"]) != "50" || string(params["total"]) != "50" {
		t.Fatalf("params = %v", params)
	}
	if string(params["lifetime_min_s"]) != "10" || string(params["lifetime_max_s"]) != "120" {
		t.Fatalf("lifetime params = %v", params)
	}

	var templates []map[string]json.RawMessage
	if err := json.Unmarshal(top["templates"], &templates); err != nil {
		t.Fatalf("templates unmarshal: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(templates))
	}
	for _, key := range []string{"template_id", "weight", "cpu_millis", "mem_mib"} {
		if _, ok := templates[0][key]; !ok {
			t.Fatalf("templates[0] missing key %q", key)
		}
	}
	if string(templates[0]["template_id"]) != `"tpl-small"` ||
		string(templates[0]["weight"]) != "1" ||
		string(templates[0]["cpu_millis"]) != "1000" ||
		string(templates[0]["mem_mib"]) != "2048" {
		t.Fatalf("templates[0] = %v", templates[0])
	}

	var requests []map[string]json.RawMessage
	if err := json.Unmarshal(top["requests"], &requests); err != nil {
		t.Fatalf("requests unmarshal: %v", err)
	}
	if len(requests) != cfg.Total {
		t.Fatalf("requests len = %d, want %d", len(requests), cfg.Total)
	}
	var prev int64 = -1
	for i, req := range requests {
		for _, key := range []string{"seq", "arrival_ms", "template_id", "cpu_millis", "mem_mib", "lifetime_ms"} {
			if _, ok := req[key]; !ok {
				t.Fatalf("requests[%d] missing key %q", i, key)
			}
		}
		var seqNo, arrival, life int64
		if err := json.Unmarshal(req["seq"], &seqNo); err != nil {
			t.Fatalf("requests[%d].seq: %v", i, err)
		}
		if err := json.Unmarshal(req["arrival_ms"], &arrival); err != nil {
			t.Fatalf("requests[%d].arrival_ms: %v", i, err)
		}
		if err := json.Unmarshal(req["lifetime_ms"], &life); err != nil {
			t.Fatalf("requests[%d].lifetime_ms: %v", i, err)
		}
		if int(seqNo) != i {
			t.Fatalf("requests[%d].seq = %d", i, seqNo)
		}
		if arrival < prev {
			t.Fatalf("requests not sorted by arrival_ms at %d: %d < %d", i, arrival, prev)
		}
		prev = arrival
		if life < 10000 || life > 120000 {
			t.Fatalf("requests[%d].lifetime_ms = %d, want within [10000, 120000]", i, life)
		}
	}
	if string(requests[0]["arrival_ms"]) != "0" {
		t.Fatalf("first arrival_ms = %s, want 0", requests[0]["arrival_ms"])
	}
}

func TestDumpTraceUnannotatedTemplateZeroSpec(t *testing.T) {
	cfg := &Config{
		Seed:      1,
		Total:     5,
		Templates: []TemplateSpec{{TemplateID: "tpl-x", Weight: 1}},
	}
	seq := GenerateSequence(cfg, rand.New(rand.NewSource(cfg.Seed)))
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := DumpTrace(path, cfg, seq); err != nil {
		t.Fatalf("DumpTrace: %v", err)
	}
	data, _ := os.ReadFile(path)
	var tf struct {
		Requests []struct {
			CpuMillis int64 `json:"cpu_millis"`
			MemMiB    int64 `json:"mem_mib"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(data, &tf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, r := range tf.Requests {
		if r.CpuMillis != 0 || r.MemMiB != 0 {
			t.Fatalf("requests[%d] spec = %d/%d, want 0/0 for unannotated template", i, r.CpuMillis, r.MemMiB)
		}
	}
}
