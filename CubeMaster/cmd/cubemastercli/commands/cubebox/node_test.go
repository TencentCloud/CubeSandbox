// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
	})

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return string(data)
}

func TestPrintNodeSummaryOmitsCapacityColumns(t *testing.T) {
	output := captureStdout(t, func() {
		printNodeSummary([]*node.Node{
			{
				InsID:         "node-1",
				IP:            "10.0.0.1",
				InstanceType:  "S5",
				Zone:          "ap-shanghai-1",
				CPUType:       "AMD",
				Healthy:       true,
				HostStatus:    "RUNNING",
				QuotaCpu:      8000,
				QuotaCpuUsage: 2000,
				QuotaMem:      16384,
				QuotaMemUsage: 4096,
				MvmNum:        12,
				Score:         0.875,
			},
		}, false)
	})

	for _, unwanted := range []string{"CPU_FREE", "MEM_FREE_MIB", "MVM_NUM", "SCORE", "6000", "12288", "12", "0.8750"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("output=%q, should not contain %q", output, unwanted)
		}
	}
	for _, wanted := range []string{"NODE_ID", "NODE_IP", "INSTANCE_TYPE", "HOST_STATUS", "node-1", "10.0.0.1", "RUNNING"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output=%q, missing %q", output, wanted)
		}
	}
}

func TestPrintNodeSummaryIncludesHostFacts(t *testing.T) {
	output := captureStdout(t, func() {
		printNodeSummary([]*node.Node{
			{
				InsID:      "node-1",
				IP:         "10.0.0.1",
				HostStatus: "RUNNING",
				HostFacts: &node.HostFacts{
					CPUVendor:             "GenuineIntel",
					CPUIDHash:             "sha256:aabbccddeeff00112233",
					HostKernelRelease:     "5.15.0",
					HostKernelFingerprint: "sha256:1122334455667788",
					KVMAPIVersion:         12,
					KVMModuleTaint:        "EO",
				},
			},
		}, false)
	})

	for _, wanted := range []string{
		"CPU_VENDOR", "CPUID", "KERNEL_REL", "KERNEL_FP", "KVM_VER", "KVM_TAINT",
		"GenuineIntel", "sha256:aabbccddeeff", "5.15.0", "sha256:112233445566", "12", "EO",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output=%q, missing %q", output, wanted)
		}
	}
	// Full 20-char cpuid hex must be truncated to 12 chars.
	if strings.Contains(output, "aabbccddeeff00112233") {
		t.Fatalf("output=%q, cpuid hash should be truncated", output)
	}
}

func TestPrintNodeSummaryHostFactsAbsent(t *testing.T) {
	output := captureStdout(t, func() {
		printNodeSummary([]*node.Node{
			{InsID: "node-1", IP: "10.0.0.1", HostStatus: "RUNNING"},
		}, false)
	})
	// Header columns are always present; missing facts render as "-".
	for _, wanted := range []string{"CPU_VENDOR", "KERNEL_REL", "KVM_TAINT"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output=%q, missing header %q", output, wanted)
		}
	}
}

func TestFormatHostFacts(t *testing.T) {
	t.Run("nil facts all dashes", func(t *testing.T) {
		vendor, cpuid, kernelRel, kernelFP, kvmVer, kvmTaint := formatHostFacts(nil)
		for _, got := range []string{vendor, cpuid, kernelRel, kernelFP, kvmVer, kvmTaint} {
			if got != "-" {
				t.Errorf("nil facts must render every cell as \"-\", got %q", got)
			}
		}
	})
	t.Run("empty fields render dash", func(t *testing.T) {
		vendor, cpuid, kernelRel, kernelFP, kvmVer, kvmTaint := formatHostFacts(&node.HostFacts{})
		for name, got := range map[string]string{
			"vendor": vendor, "cpuid": cpuid, "kernelRel": kernelRel, "kernelFP": kernelFP,
			"kvmVer": kvmVer, "kvmTaint": kvmTaint,
		} {
			if got != "-" {
				t.Errorf("empty %s must render \"-\", got %q", name, got)
			}
		}
	})
	t.Run("populated fields", func(t *testing.T) {
		vendor, cpuid, kernelRel, kernelFP, kvmVer, kvmTaint := formatHostFacts(&node.HostFacts{
			CPUVendor:             "GenuineIntel",
			CPUIDHash:             "sha256:aabbccddeeff0011",
			HostKernelRelease:     "5.15.0",
			HostKernelFingerprint: "sha256:1122334455667788",
			KVMAPIVersion:         12,
			KVMModuleTaint:        "EO",
		})
		if vendor != "GenuineIntel" {
			t.Errorf("vendor = %q", vendor)
		}
		if cpuid != "sha256:aabbccddeeff" {
			t.Errorf("cpuid = %q, want truncated", cpuid)
		}
		if kernelRel != "5.15.0" {
			t.Errorf("kernelRel = %q, want 5.15.0", kernelRel)
		}
		if kernelFP != "sha256:112233445566" {
			t.Errorf("kernelFP = %q, want truncated", kernelFP)
		}
		if kvmVer != "12" {
			t.Errorf("kvmVer = %q, want 12", kvmVer)
		}
		if kvmTaint != "EO" {
			t.Errorf("kvmTaint = %q, want EO", kvmTaint)
		}
	})
}

func TestShortHash(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "-"},
		{"sha256:aabbccddeeff00112233", "sha256:aabbccddeeff"},
		{"sha256:short", "sha256:short"},
		{"nocolonbutlongvalue", "nocolonbutlo"}, // no colon, truncated to 12 chars
		{"short", "short"},
	}
	for _, tc := range cases {
		if got := shortHash(tc.in); got != tc.want {
			t.Errorf("shortHash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintNodeSummaryScoreOnlyKeepsScoreColumns(t *testing.T) {
	output := captureStdout(t, func() {
		printNodeSummary([]*node.Node{
			{
				InsID: "node-1",
				Score: 0.875,
			},
		}, true)
	})

	for _, wanted := range []string{"NODE_ID", "SCORE", "METRIC_UPDATE", "0.8750"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output=%q, missing %q", output, wanted)
		}
	}
}
