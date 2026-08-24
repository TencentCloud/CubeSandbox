package qos

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigRejectsUnknownFields(t *testing.T) {
	var cfg Config
	err := json.Unmarshal([]byte(`{"network":{"bandwidth_mbps":100}}`), &cfg)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestConfigValidateRequiresPositiveBandwidth(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{name: "missing network", cfg: &Config{}},
		{name: "zero network qos", cfg: &Config{Network: &NetworkConfig{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfigValidateRejectsExplicitZeroPacketsPerSecond(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"network":{"bandwidthMbps":100,"packetsPerSecond":0}}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "packetsPerSecond") {
		t.Fatalf("Validate error=%v, want packetsPerSecond validation error", err)
	}
}

func TestConfigValidateRejectsExplicitZeroBandwidth(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"network":{"bandwidthMbps":0,"packetsPerSecond":5000}}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bandwidthMbps") {
		t.Fatalf("Validate error=%v, want bandwidthMbps validation error", err)
	}
}

func TestConfigValidateAcceptsBlockIOOnly(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"blockIo":{"throughputMiBps":64,"iops":1000}}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error=%v", err)
	}
	if cfg.BlockIO == nil || cfg.BlockIO.ThroughputMiBps != 64 || cfg.BlockIO.IOPS != 1000 {
		t.Fatalf("block io config=%+v, want 64 MiB/s and 1000 IOPS", cfg.BlockIO)
	}
}

func TestConfigValidateRejectsExplicitZeroBlockIOLimits(t *testing.T) {
	for _, raw := range []string{
		`{"blockIo":{"throughputMiBps":0,"iops":1000}}`,
		`{"blockIo":{"throughputMiBps":64,"iops":0}}`,
		`{"blockIo":{}}`,
	} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("Unmarshal(%s) error=%v", raw, err)
		}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestConfigValidateRejectsEmptyQos(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty qos unexpectedly validated")
	}
}

func TestMarshalAnnotationUsesCanonicalTokenBucket(t *testing.T) {
	raw, err := MarshalAnnotation(&Config{
		Network: &NetworkConfig{BandwidthMbps: 100},
	})
	if err != nil {
		t.Fatalf("MarshalAnnotation error=%v", err)
	}
	want := `{"Qos":{"BandWidth":{"Size":1250000,"OneTimeBurst":0,"RefillTime":100},"OPS":{"Size":0,"OneTimeBurst":0,"RefillTime":0}},"Version":1}`
	if raw != want {
		t.Fatalf("annotation=%s, want %s", raw, want)
	}
}

func TestMarshalAnnotationIncludesPacketsPerSecond(t *testing.T) {
	raw, err := MarshalAnnotation(&Config{
		Network: &NetworkConfig{PacketsPerSecond: 5000},
	})
	if err != nil {
		t.Fatalf("MarshalAnnotation error=%v", err)
	}
	want := `{"Qos":{"BandWidth":{"Size":0,"OneTimeBurst":0,"RefillTime":0},"OPS":{"Size":5000,"OneTimeBurst":0,"RefillTime":1000}},"Version":1}`
	if raw != want {
		t.Fatalf("annotation=%s, want %s", raw, want)
	}
}

func TestMarshalAnnotationIncludesBandwidthAndPacketsPerSecond(t *testing.T) {
	raw, err := MarshalAnnotation(&Config{
		Network: &NetworkConfig{BandwidthMbps: 100, PacketsPerSecond: 5000},
	})
	if err != nil {
		t.Fatalf("MarshalAnnotation error=%v", err)
	}
	if !strings.Contains(raw, `"Size":1250000`) || !strings.Contains(raw, `"Size":5000`) {
		t.Fatalf("annotation=%s, want bandwidth and packet buckets", raw)
	}
}

func TestMarshalBlockIOAnnotationUsesCanonicalTokenBuckets(t *testing.T) {
	raw, err := MarshalBlockIOAnnotation(&BlockIOConfig{
		ThroughputMiBps: 64,
		IOPS:            1000,
	})
	if err != nil {
		t.Fatalf("MarshalBlockIOAnnotation error=%v", err)
	}
	want := `{"bandwidth":{"size":67108864,"refill_time":1000},"ops":{"size":1000,"refill_time":1000}}`
	if raw != want {
		t.Fatalf("annotation=%s, want %s", raw, want)
	}
}

func TestParseBlockIOAnnotationReturnsConfiguredQos(t *testing.T) {
	blockIO, err := ParseBlockIOAnnotation(`{"bandwidth":{"size":67108864,"refill_time":1000},"ops":{"size":1000,"refill_time":1000}}`)
	if err != nil {
		t.Fatalf("ParseBlockIOAnnotation error=%v", err)
	}
	if blockIO == nil || blockIO.ThroughputMiBps != 64 || blockIO.IOPS != 1000 {
		t.Fatalf("block io config=%+v, want 64 MiB/s and 1000 IOPS", blockIO)
	}
}

func TestResolveTemplateQosPreservesExplicitAndOmittedBehavior(t *testing.T) {
	resources := TemplateResources{CPU: "2000m", Memory: "4Gi"}
	if got, err := ResolveTemplate(nil, resources); err != nil || got != nil {
		t.Fatalf("ResolveTemplate(nil)=(%+v, %v), want (nil, nil)", got, err)
	}

	explicit := &Config{
		Network: &NetworkConfig{BandwidthMbps: 100},
		BlockIO: &BlockIOConfig{IOPS: 1000},
	}
	got, err := ResolveTemplate(explicit, resources)
	if err != nil {
		t.Fatalf("ResolveTemplate error=%v", err)
	}
	if got == explicit || got.Network == explicit.Network || got.BlockIO == explicit.BlockIO {
		t.Fatal("ResolveTemplate must return an isolated configuration")
	}
	if got.Network.BandwidthMbps != 100 || got.BlockIO.IOPS != 1000 {
		t.Fatalf("resolved qos=%+v, want explicit limits", got)
	}
}

func TestResolveTemplateQosPassesResourcesToInferenceSeam(t *testing.T) {
	original := inferTemplateDefaults
	t.Cleanup(func() { inferTemplateDefaults = original })
	inferTemplateDefaults = func(resources TemplateResources) *Config {
		if resources.CPU != "500m" || resources.Memory != "1024Mi" {
			t.Fatalf("resources=%+v, want requested CPU and memory", resources)
		}
		return &Config{Network: &NetworkConfig{BandwidthMbps: 25}}
	}

	got, err := ResolveTemplate(nil, TemplateResources{CPU: "500m", Memory: "1024Mi"})
	if err != nil {
		t.Fatalf("ResolveTemplate error=%v", err)
	}
	if got == nil || got.Network == nil || got.Network.BandwidthMbps != 25 {
		t.Fatalf("resolved qos=%+v, want inferred bandwidth", got)
	}
}

func TestResolveTemplateQosExplicitFieldsOverrideInferredFields(t *testing.T) {
	inferred := &Config{
		Network: &NetworkConfig{BandwidthMbps: 50, PacketsPerSecond: 2000},
		BlockIO: &BlockIOConfig{ThroughputMiBps: 32, IOPS: 500},
	}
	explicit := &Config{
		Network: &NetworkConfig{BandwidthMbps: 100},
		BlockIO: &BlockIOConfig{IOPS: 1000},
	}
	got, err := resolveTemplate(explicit, inferred)
	if err != nil {
		t.Fatalf("resolveTemplate error=%v", err)
	}
	if got.Network.BandwidthMbps != 100 || got.Network.PacketsPerSecond != 2000 {
		t.Fatalf("network qos=%+v, want explicit bandwidth and inferred PPS", got.Network)
	}
	if got.BlockIO.ThroughputMiBps != 32 || got.BlockIO.IOPS != 1000 {
		t.Fatalf("block io qos=%+v, want inferred throughput and explicit IOPS", got.BlockIO)
	}
}

func TestParseAnnotationReturnsConfiguredQos(t *testing.T) {
	cfg, err := ParseAnnotation(`{"Qos":{"BandWidth":{"Size":1250000,"OneTimeBurst":0,"RefillTime":100},"OPS":{"Size":0,"OneTimeBurst":0,"RefillTime":0}},"Version":1}`)
	if err != nil {
		t.Fatalf("ParseAnnotation error=%v", err)
	}
	if cfg == nil || cfg.Network == nil || cfg.Network.BandwidthMbps != 100 {
		t.Fatalf("config=%+v, want 100 Mbps", cfg)
	}
}

func TestParseAnnotationReturnsConfiguredPacketsPerSecond(t *testing.T) {
	cfg, err := ParseAnnotation(`{"Qos":{"BandWidth":{},"OPS":{"Size":5000,"RefillTime":1000}},"Version":1}`)
	if err != nil {
		t.Fatalf("ParseAnnotation error=%v", err)
	}
	if cfg == nil || cfg.Network == nil || cfg.Network.PacketsPerSecond != 5000 {
		t.Fatalf("config=%+v, want 5000 packets per second", cfg)
	}
}

func TestParseAnnotationRejectsInvalidPacketsBucket(t *testing.T) {
	for _, raw := range []string{
		`{"Qos":{"BandWidth":{},"OPS":{"Size":5000}},"Version":1}`,
		`{"Qos":{"BandWidth":{},"OPS":{"Size":1,"RefillTime":3000}},"Version":1}`,
	} {
		if _, err := ParseAnnotation(raw); err == nil {
			t.Fatalf("expected invalid packets bucket error for %s", raw)
		}
	}
}

func TestParseAnnotationAcceptsLegacyVersionZero(t *testing.T) {
	cfg, err := ParseAnnotation(`{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}}}`)
	if err != nil {
		t.Fatalf("ParseAnnotation error=%v", err)
	}
	if cfg.Network.BandwidthMbps != 100 {
		t.Fatalf("bandwidth=%d, want 100", cfg.Network.BandwidthMbps)
	}
}

func TestParseAnnotationRejectsUnsupportedVersion(t *testing.T) {
	_, err := ParseAnnotation(`{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}},"Version":2}`)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("unexpected error: %v", err)
	}
}
