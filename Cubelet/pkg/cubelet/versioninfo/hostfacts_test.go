// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package versioninfo

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleCPUInfoIntel = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz
stepping	: 7
flags		: fpu vme de pse tsc msr pae mce cx8 sse sse2 avx avx2

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz
stepping	: 7
flags		: fpu vme de pse tsc msr pae mce cx8 sse sse2 avx avx2
`

// Same host but flags listed in a different order — the hash must be stable.
const sampleCPUInfoIntelReordered = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz
stepping	: 7
flags		: avx2 avx sse2 sse cx8 mce pae msr tsc pse de vme fpu
`

const sampleCPUInfoAMD = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7K62 48-Core Processor
stepping	: 1
flags		: fpu vme de pse tsc msr pae mce cx8 sse sse2 avx avx2
`

// Two different ARM cores (differing CPU part / implementer) that expose an
// IDENTICAL Features list. The x86 identity keys are absent, so without folding
// the ARM identity these would collapse to the same cpuid_hash.
const sampleCPUInfoARMNeoverseN1 = `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1
`

const sampleCPUInfoARMKunpeng = `processor	: 0
BogoMIPS	: 200.00
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics
CPU implementer	: 0x48
CPU architecture: 8
CPU variant	: 0x1
CPU part	: 0xd01
CPU revision	: 0
`

func writeTempCPUInfo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp cpuinfo: %v", err)
	}
	return path
}

func TestParseCPUInfo_Intel(t *testing.T) {
	path := writeTempCPUInfo(t, sampleCPUInfoIntel)
	vendor, model, hash := parseCPUInfo(path)
	if vendor != "GenuineIntel" {
		t.Errorf("vendor = %q, want GenuineIntel", vendor)
	}
	if model != "Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz" {
		t.Errorf("model = %q", model)
	}
	if hash == "" {
		t.Errorf("cpuid hash should not be empty")
	}
}

func TestParseCPUInfo_FlagOrderStable(t *testing.T) {
	h1 := mustHash(t, sampleCPUInfoIntel)
	h2 := mustHash(t, sampleCPUInfoIntelReordered)
	if h1 != h2 {
		t.Errorf("hash must be flag-order independent: %q vs %q", h1, h2)
	}
}

func TestParseCPUInfo_DistinctHostsDiffer(t *testing.T) {
	intel := mustHash(t, sampleCPUInfoIntel)
	amd := mustHash(t, sampleCPUInfoAMD)
	if intel == amd {
		t.Errorf("Intel and AMD hosts must hash differently")
	}
}

// On ARM the x86 identity keys are absent; the hash must still fold the ARM
// implementer/part identity so two different cores with the same Features list
// do not collapse to a false "compatible" (cpuid_hash is a required dimension).
func TestParseCPUInfo_ARMDistinctCoresDiffer(t *testing.T) {
	n1 := mustHash(t, sampleCPUInfoARMNeoverseN1)
	kunpeng := mustHash(t, sampleCPUInfoARMKunpeng)
	if n1 == kunpeng {
		t.Errorf("distinct ARM cores with identical Features must hash differently")
	}
}

func TestParseCPUInfo_ARMPopulatesHash(t *testing.T) {
	path := writeTempCPUInfo(t, sampleCPUInfoARMNeoverseN1)
	vendor, _, hash := parseCPUInfo(path)
	if vendor != "" {
		t.Errorf("ARM has no vendor_id, got %q", vendor)
	}
	if hash == "" {
		t.Errorf("ARM cpuid hash must be populated from implementer/part/features")
	}
}

func TestParseCPUInfo_MissingFile(t *testing.T) {
	vendor, model, hash := parseCPUInfo(filepath.Join(t.TempDir(), "nope"))
	if vendor != "" || model != "" || hash != "" {
		t.Errorf("missing cpuinfo should yield empty facts, got %q %q %q", vendor, model, hash)
	}
}

func TestCollectHostFacts_GracefulDegradation(t *testing.T) {
	// uname failure must not panic; fields stay zero. KVMAPIVersion is no longer
	// part of the static collection (it is re-probed per call by CollectHostFacts).
	path := writeTempCPUInfo(t, sampleCPUInfoIntel)
	facts := collectStaticHostFacts(path, "", func() string { return "" })
	if facts.CPUVendor != "GenuineIntel" {
		t.Errorf("vendor = %q", facts.CPUVendor)
	}
	if facts.HostKernelRelease != "" {
		t.Errorf("kernel release should be empty on failure, got %q", facts.HostKernelRelease)
	}
	if facts.KVMAPIVersion != 0 {
		t.Errorf("static facts must not carry a kvm version, got %d", facts.KVMAPIVersion)
	}
}

func TestCollectHostFacts_FullFacts(t *testing.T) {
	path := writeTempCPUInfo(t, sampleCPUInfoIntel)
	facts := collectStaticHostFacts(path, "", func() string { return "5.15.0-generic" })
	if facts.HostKernelRelease != "5.15.0-generic" {
		t.Errorf("kernel release = %q", facts.HostKernelRelease)
	}
	if facts.CPUIDHash == "" {
		t.Errorf("cpuid hash should be populated")
	}
}

func TestNormaliseCmdline_StripsVolatileAndSorts(t *testing.T) {
	// Two hosts with different root UUID / boot image but identical behaviour
	// params must normalise to the same string, regardless of ordering.
	a := "BOOT_IMAGE=/vmlinuz-a root=UUID=aaaa mitigations=off iommu=pt hugepagesz=1G"
	b := "root=UUID=bbbb hugepagesz=1G BOOT_IMAGE=/boot/vmlinuz-b iommu=pt mitigations=off"
	if got := normaliseCmdline(a); got != normaliseCmdline(b) {
		t.Errorf("same-behaviour cmdlines must normalise equal:\n a=%q\n b=%q", normaliseCmdline(a), normaliseCmdline(b))
	}
	// A real behaviour difference must survive normalisation.
	c := "root=UUID=cccc mitigations=auto iommu=pt hugepagesz=1G"
	if normaliseCmdline(a) == normaliseCmdline(c) {
		t.Errorf("differing mitigations must not normalise equal")
	}
}

func TestHashKernelIdentity(t *testing.T) {
	if hashKernelIdentity("", "") != "" {
		t.Errorf("all-empty inputs must hash to empty string")
	}
	base := hashKernelIdentity("5.15.0", "root=UUID=x mitigations=off")
	// Volatile-only change (root UUID) must not change the digest.
	sameBehaviour := hashKernelIdentity("5.15.0", "root=UUID=y mitigations=off")
	if base != sameBehaviour {
		t.Errorf("root UUID change must not affect kernel fingerprint")
	}
	// A behaviour param change must change the digest.
	if base == hashKernelIdentity("5.15.0", "root=UUID=x mitigations=auto") {
		t.Errorf("mitigations change must change the kernel fingerprint")
	}
	// The global taint mask must NOT be part of the fingerprint: a differing
	// non-KVM driver stack (which taints the global mask) must not split hosts.
	if base != hashKernelIdentity("5.15.0", "root=UUID=x mitigations=off") {
		t.Errorf("kernel fingerprint must be independent of the global taint mask")
	}
}

func TestCollectStaticHostFacts_KernelFingerprint(t *testing.T) {
	path := writeTempCPUInfo(t, sampleCPUInfoIntel)
	cmdline := writeTempFile(t, "cmdline", "root=UUID=x mitigations=off\n")
	facts := collectStaticHostFacts(path, cmdline, func() string { return "5.15.0" })
	if facts.HostKernelFingerprint == "" {
		t.Errorf("fingerprint should be populated from release + cmdline")
	}
	// No release and no cmdline → empty fingerprint.
	empty := collectStaticHostFacts(path, filepath.Join(t.TempDir(), "nope"), func() string { return "" })
	if empty.HostKernelFingerprint != "" {
		t.Errorf("fingerprint should be empty when release and cmdline absent, got %q", empty.HostKernelFingerprint)
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp %s: %v", name, err)
	}
	return path
}

func TestReadCmdline_NormalisesNulAndWhitespace(t *testing.T) {
	// The kernel exposes cmdline NUL-separated with a trailing newline.
	path := writeTempFile(t, "cmdline", "BOOT_IMAGE=/vmlinuz\x00root=UUID=x\x00mitigations=off\n")
	got := readCmdline(path)
	want := "BOOT_IMAGE=/vmlinuz root=UUID=x mitigations=off"
	if got != want {
		t.Errorf("readCmdline = %q, want %q", got, want)
	}
}

func TestReadCmdline_MissingFile(t *testing.T) {
	if got := readCmdline(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("missing cmdline must read as empty, got %q", got)
	}
}

// writeFakeModule creates /sys/module-style entries for one module: a map of
// field name -> content. Missing fields are simply not written.
func writeFakeModule(t *testing.T, sysDir, name string, fields map[string]string) {
	t.Helper()
	dir := filepath.Join(sysDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir module %s: %v", name, err)
	}
	for k, v := range fields {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", name, k, err)
		}
	}
}

func TestCollectKVMModuleState_CleanFamily(t *testing.T) {
	dir := t.TempDir()
	// KVM family plus an unrelated module that must be ignored.
	writeFakeModule(t, dir, "kvm", map[string]string{
		"srcversion": "AAAA", "initstate": "live", "coresize": "1150976", "initsize": "0", "taint": "",
	})
	writeFakeModule(t, dir, "kvm_pvm", map[string]string{
		"srcversion": "BBBB", "initstate": "live", "coresize": "45056", "initsize": "0",
	})
	writeFakeModule(t, dir, "ext4", map[string]string{"srcversion": "ZZZZ"})

	fp, taint, scanned := collectKVMModuleState(dir)
	if fp == "" {
		t.Errorf("fingerprint must be populated for a loaded KVM family")
	}
	if taint != "" {
		t.Errorf("clean modules must yield empty taint, got %q", taint)
	}
	if !scanned {
		t.Errorf("a readable /sys/module must report scanned=true")
	}
}

func TestCollectKVMModuleState_DetectsSuspiciousTaint(t *testing.T) {
	dir := t.TempDir()
	// O (out-of-tree) on the core module, E (unsigned) on a sibling. Result must
	// be the sorted, deduplicated union: "EO".
	writeFakeModule(t, dir, "kvm", map[string]string{
		"srcversion": "AAAA", "initstate": "live", "taint": "O",
	})
	writeFakeModule(t, dir, "kvm_intel", map[string]string{
		"srcversion": "CCCC", "initstate": "live", "taint": "E",
	})

	_, taint, _ := collectKVMModuleState(dir)
	if taint != "EO" {
		t.Errorf("taint = %q, want %q (sorted union of suspicious flags)", taint, "EO")
	}
}

func TestCollectKVMModuleState_IgnoresBenignTaint(t *testing.T) {
	dir := t.TempDir()
	// "P" (proprietary) is not in the suspicious set and must be dropped.
	writeFakeModule(t, dir, "kvm", map[string]string{"srcversion": "AAAA", "taint": "P"})
	if _, taint, _ := collectKVMModuleState(dir); taint != "" {
		t.Errorf("benign taint must be ignored, got %q", taint)
	}
}

func TestCollectKVMModuleState_FingerprintChangesOnRebuild(t *testing.T) {
	a := t.TempDir()
	writeFakeModule(t, a, "kvm", map[string]string{"srcversion": "AAAA", "initstate": "live"})
	b := t.TempDir()
	writeFakeModule(t, b, "kvm", map[string]string{"srcversion": "DIFFERENT", "initstate": "live"})

	fpA, _, _ := collectKVMModuleState(a)
	fpB, _, _ := collectKVMModuleState(b)
	if fpA == fpB {
		t.Errorf("a different srcversion must change the module fingerprint")
	}
}

func TestCollectKVMModuleState_NoKVM(t *testing.T) {
	dir := t.TempDir()
	writeFakeModule(t, dir, "ext4", map[string]string{"srcversion": "ZZZZ"})
	// A readable /sys/module with no KVM family: empty state but scanned=true, so
	// the master can authoritatively clear a stale taint (module unloaded).
	fp, taint, scanned := collectKVMModuleState(dir)
	if fp != "" || taint != "" {
		t.Errorf("absent KVM family must yield empty state, got fp=%q taint=%q", fp, taint)
	}
	if !scanned {
		t.Errorf("a readable sysfs dir with no KVM must still report scanned=true")
	}
	// Missing /sys/module entirely: a read gap, so scanned=false so the master
	// preserves the previously-known module state instead of clearing it.
	if fp, taint, scanned := collectKVMModuleState(filepath.Join(dir, "nope")); fp != "" || taint != "" || scanned {
		t.Errorf("missing sysfs dir must yield empty unscanned state, got fp=%q taint=%q scanned=%v", fp, taint, scanned)
	}
}

// End-to-end through the exported CollectHostFacts: with all /proc and /sys
// sources overridden the returned facts must carry a populated kernel
// fingerprint plus KVM module fingerprint that folds in live module state.
func TestCollectHostFacts_EndToEnd(t *testing.T) {
	origCPU, origCmd, origSys := procCPUInfoPath, procCmdlinePath, sysModulePath
	origStatic, origCached := hostFactsStatic, hostFactsCached
	t.Cleanup(func() {
		procCPUInfoPath, procCmdlinePath, sysModulePath = origCPU, origCmd, origSys
		hostFactsMu.Lock()
		hostFactsStatic, hostFactsCached = origStatic, origCached
		hostFactsMu.Unlock()
	})

	procCPUInfoPath = writeTempCPUInfo(t, sampleCPUInfoIntel)
	procCmdlinePath = writeTempFile(t, "cmdline", "root=UUID=x mitigations=off\n")
	sysModulePath = t.TempDir()
	writeFakeModule(t, sysModulePath, "kvm", map[string]string{
		"srcversion": "AAAA", "initstate": "live", "coresize": "1150976", "taint": "",
	})
	hostFactsMu.Lock()
	hostFactsCached = false
	hostFactsMu.Unlock()

	facts := CollectHostFacts()
	if facts.CPUVendor != "GenuineIntel" {
		t.Errorf("vendor = %q", facts.CPUVendor)
	}
	if facts.CPUIDHash == "" {
		t.Errorf("cpuid hash must be populated")
	}
	if facts.HostKernelFingerprint == "" {
		t.Errorf("kernel fingerprint must be populated")
	}
	if facts.KVMModuleFingerprint == "" {
		t.Errorf("kvm module fingerprint must be populated")
	}
	if facts.KVMModuleTaint != "" {
		t.Errorf("clean kvm module must yield empty taint, got %q", facts.KVMModuleTaint)
	}
}

// A first collection that raced an unreadable /proc/cpuinfo + /proc/cmdline must
// not be cached for the process lifetime: a later call, once the sources are
// readable, must re-collect and return populated facts.
func TestCollectHostFacts_RecollectsAfterEmptyFirstCollection(t *testing.T) {
	origCPU, origCmd, origSys := procCPUInfoPath, procCmdlinePath, sysModulePath
	origStatic, origCached := hostFactsStatic, hostFactsCached
	t.Cleanup(func() {
		procCPUInfoPath, procCmdlinePath, sysModulePath = origCPU, origCmd, origSys
		hostFactsMu.Lock()
		hostFactsStatic, hostFactsCached = origStatic, origCached
		hostFactsMu.Unlock()
	})

	sysModulePath = t.TempDir()

	// Simulate a cached all-empty first collection (sources raced unreadable).
	hostFactsMu.Lock()
	hostFactsStatic = HostFacts{}
	hostFactsCached = true
	hostFactsMu.Unlock()

	// Now the sources are readable; the next call must re-collect rather than
	// serving the cached empty struct.
	procCPUInfoPath = writeTempCPUInfo(t, sampleCPUInfoIntel)
	procCmdlinePath = writeTempFile(t, "cmdline", "root=UUID=x\n")

	facts := CollectHostFacts()
	if facts.CPUIDHash == "" || facts.CPUVendor != "GenuineIntel" {
		t.Fatalf("cached empty static facts must be re-collected, got %+v", facts)
	}
}

// The KVM ABI version must be re-probed on every CollectHostFacts call, not
// latched from the first collection: /dev/kvm can appear after the cubelet
// starts (on-demand kvm.ko load), so a first collection that saw 0 must not
// pin kvm_api_version to 0 for the process lifetime.
func TestCollectHostFacts_KVMAPIVersionReprobedPerCall(t *testing.T) {
	origCPU, origCmd, origSys, origKVM := procCPUInfoPath, procCmdlinePath, sysModulePath, kvmAPIVersionFn
	origStatic, origCached := hostFactsStatic, hostFactsCached
	t.Cleanup(func() {
		procCPUInfoPath, procCmdlinePath, sysModulePath, kvmAPIVersionFn = origCPU, origCmd, origSys, origKVM
		hostFactsMu.Lock()
		hostFactsStatic, hostFactsCached = origStatic, origCached
		hostFactsMu.Unlock()
	})

	procCPUInfoPath = writeTempCPUInfo(t, sampleCPUInfoIntel)
	procCmdlinePath = writeTempFile(t, "cmdline", "root=UUID=x\n")
	sysModulePath = t.TempDir()
	hostFactsMu.Lock()
	hostFactsCached = false
	hostFactsMu.Unlock()

	// First collection: /dev/kvm absent → version 0, but CPU/kernel facts land,
	// so the static set is cached.
	kvmAPIVersionFn = func() int { return 0 }
	if v := CollectHostFacts().KVMAPIVersion; v != 0 {
		t.Fatalf("first collection kvm version = %d, want 0", v)
	}

	// kvm.ko loads later; a subsequent collection must observe the new version
	// rather than serving the cached 0.
	kvmAPIVersionFn = func() int { return 12 }
	if v := CollectHostFacts().KVMAPIVersion; v != 12 {
		t.Fatalf("kvm version must be re-probed per call, got %d, want 12", v)
	}
}

func mustHash(t *testing.T, content string) string {
	t.Helper()
	_, _, hash := parseCPUInfo(writeTempCPUInfo(t, content))
	if hash == "" {
		t.Fatalf("hash unexpectedly empty")
	}
	return hash
}
