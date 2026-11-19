// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestConfig(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s3.cfg")
	if err := os.WriteFile(p, []byte("access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

func TestChunkProfilePicksTheChunkDefaults(t *testing.T) {
	cases := []struct {
		extra          string
		rootfs, memory int64
	}{
		{"", 16, 4},
		{"chunk_profile=remote\n", 16, 4},
		{"chunk_profile=Low_Latency\n", 4, 1},
		{"chunk_profile=remote\nmemory_chunk_mib=2\nrootfs_chunk_mib=8\n", 8, 2},
		{"chunk_profile=low_latency\nmemory_chunk_mib=4\n", 4, 4},
	}
	for _, tc := range cases {
		cfg, err := loadTestConfig(t, tc.extra)
		if err != nil {
			t.Fatalf("%q: %v", tc.extra, err)
		}
		rootfs, memory := cfg.ChunkSizes()
		if rootfs != tc.rootfs<<20 || memory != tc.memory<<20 {
			t.Errorf("%q: chunks %d/%d MiB, want %d/%d", tc.extra, rootfs>>20, memory>>20, tc.rootfs, tc.memory)
		}
		if cfg.SweepRate() != DefaultSweepRequestsPerSecond {
			t.Errorf("%q: sweep rate %d", tc.extra, cfg.SweepRate())
		}
	}
}

func TestTransferWorkersDefaultTo32(t *testing.T) {
	cfg, err := loadTestConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if e, r := cfg.TransferWorkers(); e != 32 || r != 32 {
		t.Fatalf("default workers %d/%d, want 32/32", e, r)
	}
	cfg, err = loadTestConfig(t, "chunk_profile=low_latency\n")
	if err != nil {
		t.Fatal(err)
	}
	if e, r := cfg.TransferWorkers(); e != 32 || r != 32 {
		t.Fatalf("low-latency workers %d/%d, want 32/32", e, r)
	}
	cfg, err = loadTestConfig(t, "export_workers=8\nread_workers=64\n")
	if err != nil {
		t.Fatal(err)
	}
	if e, r := cfg.TransferWorkers(); e != 8 || r != 64 {
		t.Fatalf("configured workers %d/%d, want 8/64", e, r)
	}
	if cfg, err := loadTestConfig(t, "sweep_requests_per_second=12\n"); err != nil || cfg.SweepRate() != 12 {
		t.Fatalf("sweep_requests_per_second=12: %+v %v", cfg, err)
	}
}

func TestInvalidTransferSettingsAreRefused(t *testing.T) {
	for _, bad := range []string{
		"chunk_profile=fast\n", "chunk_profile=requests\n", "chunk_profile=bandwidth\n",
		"export_workers=0\n", "read_workers=65\n", "export_workers=many\n",
		"memory_chunk_mib=0\n", "rootfs_chunk_mib=512\n",
		"sweep_requests_per_second=0\n", "sweep_requests_per_second=fast\n", "sweep_requests_per_second=5000\n",
	} {
		if _, err := loadTestConfig(t, bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if err := (&Config{Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s", ReadWorkers: 100}).Validate(); err == nil {
		t.Error("100 read workers passed Validate")
	}
}
