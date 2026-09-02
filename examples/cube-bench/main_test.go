package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPrepareHostMountCompactsValidArray(t *testing.T) {
	got, err := prepareHostMount(`[
		{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}
	]`)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}

	want := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	if got != want {
		t.Fatalf("prepareHostMount=%q, want %q", got, want)
	}
}

func TestPrepareHostMountPreservesCompactedArray(t *testing.T) {
	got, err := prepareHostMount(`[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}

	want := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	if got != want {
		t.Fatalf("prepareHostMount=%q, want %q", got, want)
	}
}

func TestPrepareHostMountRejectsInvalidJSON(t *testing.T) {
	if _, err := prepareHostMount(`[{"hostPath":]`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want invalid JSON error")
	}
}

func TestPrepareHostMountRejectsNonArrayJSON(t *testing.T) {
	if _, err := prepareHostMount(`{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want non-array error")
	}
}

func TestPrepareHostMountAllowsEmptyInput(t *testing.T) {
	got, err := prepareHostMount("")
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("prepareHostMount returned %q, want empty string", got)
	}
}

func TestPrepareHostMountRejectsEmptyArray(t *testing.T) {
	if _, err := prepareHostMount(`[]`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want empty array error")
	}
}

func TestParseNetworkPolicy(t *testing.T) {
	got, err := parseNetworkPolicy("rules")
	if err != nil {
		t.Fatalf("parseNetworkPolicy(rules): %v", err)
	}
	if got != networkPolicyRules {
		t.Fatalf("got %q, want %q", got, networkPolicyRules)
	}

	got, err = parseNetworkPolicy("")
	if err != nil {
		t.Fatalf("parseNetworkPolicy(\"\"): %v", err)
	}
	if got != networkPolicyNone {
		t.Fatalf("got %q, want %q", got, networkPolicyNone)
	}

	if _, err := parseNetworkPolicy("stress"); err == nil {
		t.Fatal("parseNetworkPolicy(stress) returned nil error, want rejection")
	}
}

func TestBuildCreateRequestBodyNoneOmitsNetwork(t *testing.T) {
	raw, err := buildCreateRequestBody("tpl-1", "", networkPolicyNone)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["allow_internet_access"]; ok {
		t.Fatalf("none policy must omit allow_internet_access, got %s", body["allow_internet_access"])
	}
	if _, ok := body["network"]; ok {
		t.Fatalf("none policy must omit network, got %s", body["network"])
	}
}

func TestBuildCreateRequestBodyRulesShape(t *testing.T) {
	raw, err := buildCreateRequestBody("tpl-1", "", networkPolicyRules)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body struct {
		TemplateID          string `json:"templateID"`
		AllowInternetAccess *bool  `json:"allow_internet_access"`
		Network             *struct {
			AllowOut []string `json:"allowOut"`
			Rules    []struct {
				Name   string `json:"name"`
				Action struct {
					Allow  bool `json:"allow"`
					Inject []struct {
						Header string `json:"header"`
						Secret string `json:"secret"`
					} `json:"inject"`
				} `json:"action"`
			} `json:"rules"`
		} `json:"network"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.TemplateID != "tpl-1" {
		t.Fatalf("templateID=%q, want tpl-1", body.TemplateID)
	}
	if body.AllowInternetAccess == nil || *body.AllowInternetAccess {
		t.Fatalf("allow_internet_access=%v, want false", body.AllowInternetAccess)
	}
	if body.Network == nil {
		t.Fatal("network missing")
	}

	fp := networkFingerprint(networkPolicyRules)
	if len(body.Network.AllowOut) != fp.AllowOut {
		t.Fatalf("allowOut count=%d, want %d", len(body.Network.AllowOut), fp.AllowOut)
	}
	if len(body.Network.Rules) != fp.Rules {
		t.Fatalf("rules count=%d, want %d", len(body.Network.Rules), fp.Rules)
	}

	injectRules := 0
	for _, r := range body.Network.Rules {
		if len(r.Action.Inject) > 0 {
			injectRules++
		}
	}
	if injectRules != fp.InjectRules {
		t.Fatalf("inject rules=%d, want %d", injectRules, fp.InjectRules)
	}
	if fp.AllowOut != 24 || fp.Rules != 6 || fp.InjectRules != 2 {
		t.Fatalf("unexpected fingerprint: %+v", fp)
	}
}

func TestBuildCreateRequestBodyRulesKeepsHostMount(t *testing.T) {
	hostMount := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	raw, err := buildCreateRequestBody("tpl-1", hostMount, networkPolicyRules)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body struct {
		Metadata map[string]string `json:"metadata"`
		Network  *struct{}         `json:"network"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Metadata["host-mount"] != hostMount {
		t.Fatalf("host-mount=%q, want %q", body.Metadata["host-mount"], hostMount)
	}
	if body.Network == nil {
		t.Fatal("network missing when host-mount is set")
	}
}

func TestBuildCreateRequestBodyWithTimeout(t *testing.T) {
	timeout := int64(70)
	raw, err := buildCreateRequestBodyWithTimeout("tpl-ttl", "", networkPolicyNone, &timeout)
	if err != nil {
		t.Fatalf("buildCreateRequestBodyWithTimeout: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(body["timeout"]) != "70" {
		t.Fatalf("timeout = %s, want 70", body["timeout"])
	}
	if string(body["templateID"]) != `"tpl-ttl"` {
		t.Fatalf("templateID = %s, want tpl-ttl", body["templateID"])
	}

	// nil timeout must omit the field entirely (legacy behavior).
	raw, err = buildCreateRequestBody("tpl-legacy", "", networkPolicyNone)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := legacy["timeout"]; ok {
		t.Fatalf("legacy body must omit timeout, got %s", legacy["timeout"])
	}
}

func scheduledExportFixture() (*Config, []IterResult) {
	cfg := &Config{
		Template:       "tpl-a",
		Mode:           "create-delete",
		Seed:           42,
		Workload:       "burst",
		Rate:           50,
		hasLifetime:    true,
		LifetimeMin:    10,
		LifetimeMax:    120,
		Scheduled:      true,
		Templates:      []TemplateSpec{{TemplateID: "tpl-a", Weight: 1, CpuMillis: 1000, MemMiB: 2048}},
		requestHeaders: map[string]string{},
		elapsed:        12.5,
	}
	results := []IterResult{
		{Seq: 0, CreateMs: 100, DeleteMs: 40, TemplateID: "tpl-a", ScheduledArrivalMs: 0, ActualStartMs: 1, SchedDelayMs: 1, LifetimeMs: 53210},
		{Seq: 1, CreateMs: 120, DeleteMs: 41, TemplateID: "tpl-a", ScheduledArrivalMs: 20, ActualStartMs: 23, SchedDelayMs: 3, LifetimeMs: 61000},
		{Seq: 2, CreateMs: 0, Err: "boom", TemplateID: "tpl-a", ScheduledArrivalMs: 40, ActualStartMs: 45, SchedDelayMs: 5, LifetimeMs: 70000},
	}
	return cfg, results
}

func TestExportJSONScheduledAddsKeys(t *testing.T) {
	cfg, results := scheduledExportFixture()
	cfg.setDispatchElapsed(2.5)
	cfg.Output = t.TempDir() + "/report.json"

	exportJSON(results, cfg)

	data, err := os.ReadFile(cfg.Output)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	config := report["config"].(map[string]any)
	if config["workload"] != "burst" {
		t.Fatalf("config.workload = %v", config["workload"])
	}
	if config["seed"].(float64) != 42 || config["rate_per_sec"].(float64) != 50 {
		t.Fatalf("config seed/rate = %v/%v", config["seed"], config["rate_per_sec"])
	}
	if config["lifetime_min_s"].(float64) != 10 || config["lifetime_max_s"].(float64) != 120 {
		t.Fatalf("config lifetime = %v/%v", config["lifetime_min_s"], config["lifetime_max_s"])
	}
	templates := config["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("config.templates len = %d", len(templates))
	}
	tpl := templates[0].(map[string]any)
	if tpl["template_id"] != "tpl-a" || tpl["weight"].(float64) != 1 || tpl["cpu_millis"].(float64) != 1000 || tpl["mem_mib"].(float64) != 2048 {
		t.Fatalf("config.templates[0] = %v", tpl)
	}

	summary := report["summary"].(map[string]any)
	// Legacy keys must remain.
	for _, key := range []string{"total_time_s", "successful", "errors", "success_rate", "throughput_qps"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("summary missing legacy key %q", key)
		}
	}
	for _, key := range []string{"queue_delay_p50_ms", "queue_delay_p95_ms", "queue_delay_p99_ms", "dispatch_window_s", "dispatch_qps"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("summary missing key %q", key)
		}
	}
	if summary["dispatch_window_s"].(float64) != 2.5 {
		t.Fatalf("dispatch_window_s = %v, want 2.5", summary["dispatch_window_s"])
	}
	if got := summary["dispatch_qps"].(float64); got != 1.2 {
		t.Fatalf("dispatch_qps = %v, want 1.2 (3 requests / 2.5s)", got)
	}
	// Delays of 1/3/5ms against a 20ms inter-arrival: not saturated, no marker.
	if _, ok := summary["dispatch_saturated"]; ok {
		t.Fatalf("unsaturated run must not set dispatch_saturated")
	}
	if summary["queue_delay_p50_ms"].(float64) != 3 {
		t.Fatalf("queue_delay_p50_ms = %v, want 3", summary["queue_delay_p50_ms"])
	}
	perTemplate := summary["per_template"].(map[string]any)
	agg := perTemplate["tpl-a"].(map[string]any)
	if agg["attempts"].(float64) != 3 || agg["created"].(float64) != 2 {
		t.Fatalf("per_template tpl-a = %v", agg)
	}
	if got := agg["success_rate"].(float64); got < 0.66 || got > 0.67 {
		t.Fatalf("per_template success_rate = %v, want ~0.667", got)
	}

	raw := report["raw"].([]any)
	first := raw[0].(map[string]any)
	for _, key := range []string{"template_id", "scheduled_arrival_ms", "actual_start_ms", "sched_delay_ms", "lifetime_ms"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("raw[0] missing key %q", key)
		}
	}
	if first["template_id"] != "tpl-a" || first["lifetime_ms"].(float64) != 53210 {
		t.Fatalf("raw[0] = %v", first)
	}
}

func TestExportJSONLegacyOmitsScheduledKeys(t *testing.T) {
	cfg := &Config{
		Template:       "tpl-a",
		Mode:           "create-delete",
		requestHeaders: map[string]string{},
		elapsed:        5,
	}
	cfg.Output = t.TempDir() + "/report.json"
	results := []IterResult{{Seq: 1, CreateMs: 100, DeleteMs: 40}}

	exportJSON(results, cfg)

	data, err := os.ReadFile(cfg.Output)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	config := report["config"].(map[string]any)
	for _, key := range []string{"workload", "seed", "rate_per_sec", "lifetime_min_s", "lifetime_max_s", "templates"} {
		if _, ok := config[key]; ok {
			t.Fatalf("legacy config must not contain %q", key)
		}
	}
	summary := report["summary"].(map[string]any)
	for _, key := range []string{"queue_delay_p50_ms", "queue_delay_p95_ms", "queue_delay_p99_ms", "per_template", "dispatch_window_s", "dispatch_qps"} {
		if _, ok := summary[key]; ok {
			t.Fatalf("legacy summary must not contain %q", key)
		}
	}
	raw := report["raw"].([]any)
	first := raw[0].(map[string]any)
	for _, key := range []string{"template_id", "scheduled_arrival_ms", "actual_start_ms", "sched_delay_ms", "lifetime_ms"} {
		if _, ok := first[key]; ok {
			t.Fatalf("legacy raw[0] must not contain %q", key)
		}
	}
}

func TestExportJSONScheduledASAPOmitsDispatchKeys(t *testing.T) {
	cfg, results := scheduledExportFixture()
	cfg.Rate = 0 // ASAP dispatch: no arrival pacing, dispatch window is meaningless
	cfg.setDispatchElapsed(0.01)
	cfg.Output = t.TempDir() + "/report.json"

	exportJSON(results, cfg)

	data, err := os.ReadFile(cfg.Output)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	summary := report["summary"].(map[string]any)
	for _, key := range []string{"dispatch_window_s", "dispatch_qps"} {
		if _, ok := summary[key]; ok {
			t.Fatalf("unpaced scheduled summary must not contain %q", key)
		}
	}
	// Queue-delay keys are still meaningful without pacing.
	if _, ok := summary["queue_delay_p50_ms"]; !ok {
		t.Fatalf("summary missing key %q", "queue_delay_p50_ms")
	}
}

func TestExportJSONSaturatedMarked(t *testing.T) {
	cfg, results := scheduledExportFixture()
	cfg.setDispatchElapsed(2.5)
	// p95 100ms against a 20ms inter-arrival (50/s): the dispatcher fell
	// permanently behind, so the export must carry the saturation marker.
	for i := range results {
		results[i].SchedDelayMs = 100
	}
	cfg.Output = t.TempDir() + "/report.json"

	exportJSON(results, cfg)

	data, err := os.ReadFile(cfg.Output)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	summary := report["summary"].(map[string]any)
	if got := summary["dispatch_saturated"]; got != float64(1) {
		t.Fatalf("dispatch_saturated = %v, want 1", got)
	}
}

func TestDispatchSaturated(t *testing.T) {
	cfg, results := scheduledExportFixture()

	// Legacy mode and unpaced runs never report saturation.
	if sat, _, _ := dispatchSaturated([]IterResult{{SchedDelayMs: 1e6}}, &Config{}); sat {
		t.Errorf("legacy config reported saturated")
	}
	asap, _ := scheduledExportFixture()
	asap.Rate = 0
	if sat, _, _ := dispatchSaturated(results, asap); sat {
		t.Errorf("ASAP run reported saturated")
	}

	if sat, p95, ia := dispatchSaturated(results, cfg); sat {
		t.Errorf("fixture delays (p95 %v vs inter-arrival %v) reported saturated", p95, ia)
	}
	for i := range results {
		results[i].SchedDelayMs = 100
	}
	sat, p95, ia := dispatchSaturated(results, cfg)
	if !sat || p95 <= ia {
		t.Errorf("saturated=%v p95=%v inter-arrival=%v, want saturated with p95 > inter-arrival", sat, p95, ia)
	}
}

func TestDispatchSaturatedToleratesTransient(t *testing.T) {
	cfg, _ := scheduledExportFixture() // rate 50/s -> 20ms inter-arrival
	// 100 requests: 4 delayed far past the inter-arrival gap (a transient
	// burst), the rest on time. p95 must stay under the gap, so the
	// whole-run saturation marker stays off.
	results := make([]IterResult, 100)
	for i := range results {
		results[i] = IterResult{Seq: i, SchedDelayMs: 1}
	}
	for _, i := range []int{10, 30, 60, 85} {
		results[i].SchedDelayMs = 500
	}
	if sat, p95, ia := dispatchSaturated(results, cfg); sat {
		t.Errorf("transient burst tripped the saturation marker (p95=%v, inter-arrival=%v)", p95, ia)
	}
}
