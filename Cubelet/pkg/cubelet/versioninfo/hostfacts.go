// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package versioninfo

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// HostFacts captures the host-level identity that gates cross-node snapshot
// restore: CPU feature set, host kernel identity, and KVM ABI version. These are
// distinct from the guest-environment component versions collected by Collector
// — they describe the physical host that runs the hypervisor.
//
// HostKernelFingerprint folds the running kernel release and the (normalised)
// boot cmdline into one digest: two hosts are kernel-compatible only when both
// agree. HostKernelRelease is kept alongside for human-readable debugging.
//
// The global kernel taint mask is deliberately NOT part of this fingerprint: it
// reflects every loaded module (a proprietary NIC or storage driver taints it),
// so folding it in would report two otherwise-identical hosts incompatible over
// unrelated driver stacks. KVM-module tampering is covered separately by
// KVMModuleTaint, which is the signal the control plane actually gates on.
//
// KVMModuleFingerprint / KVMModuleTaint describe the loaded KVM kernel modules
// (kvm plus its per-arch/per-variant siblings such as kvm_intel, kvm_amd,
// kvm_pvm). The fingerprint is an equality signal (identical build + init state)
// while the taint string is an absolute integrity signal: a non-empty value
// means at least one KVM module carries a suspicious taint (unsigned / forced /
// out-of-tree), which the control plane treats as a hard restore blocker.
//
// KVMModuleScanned reports whether /sys/module was successfully read this
// collection. It lets the control plane distinguish a host that genuinely has no
// KVM module loaded (Scanned=true with empty fingerprint/taint — authoritative)
// from a transient read gap (Scanned=false), so an unloaded module can clear a
// stale taint instead of latching it forever. It is a per-heartbeat collection
// signal, not a persisted fact.
type HostFacts struct {
	CPUVendor             string `json:"cpu_vendor,omitempty"`
	CPUModel              string `json:"cpu_model,omitempty"`
	CPUIDHash             string `json:"cpuid_hash,omitempty"`
	HostKernelRelease     string `json:"host_kernel_release,omitempty"`
	HostKernelFingerprint string `json:"host_kernel_fingerprint,omitempty"`
	KVMAPIVersion         int    `json:"kvm_api_version,omitempty"`
	KVMModuleFingerprint  string `json:"kvm_module_fingerprint,omitempty"`
	KVMModuleTaint        string `json:"kvm_module_taint,omitempty"`
	KVMModuleScanned      bool   `json:"kvm_module_scanned,omitempty"`
}

// kvmGetAPIVersion is the KVM_GET_API_VERSION ioctl: _IO(KVMIO, 0x00) with
// KVMIO == 0xAE. It takes no argument and returns the stable KVM ABI version
// (12 on all modern kernels).
const kvmGetAPIVersion = 0xAE00

const devKVMPath = "/dev/kvm"

var (
	hostFactsMu     sync.Mutex
	hostFactsStatic HostFacts
	hostFactsCached bool
)

// CollectHostFacts gathers host CPU/kernel/KVM facts. The boot-stable sources
// (CPU feature set, kernel release + cmdline fingerprint) are read once and
// cached — they are fixed for the lifetime of the process. The KVM ABI version
// and KVM module state, by contrast, are runtime-variable: kvm.ko can be loaded
// on demand after the cubelet starts (modprobe / systemd ordering race), so
// /dev/kvm and /sys/module are re-probed on every call rather than latched from
// the first collection. Every source degrades gracefully — a missing /proc or
// /sys path, uname failure, or absent /dev/kvm leaves the corresponding input
// empty rather than failing collection.
func CollectHostFacts() HostFacts {
	hostFactsMu.Lock()
	// Cache the boot-stable facts on first success only. If the first collection
	// hit a momentarily-unreadable /proc/cpuinfo or /proc/cmdline (e.g. very early
	// startup) it would otherwise cache an all-empty result for the process
	// lifetime; re-collecting until a non-empty result lands makes it robust.
	if !hostFactsCached || staticFactsEmpty(hostFactsStatic) {
		hostFactsStatic = collectStaticHostFacts(
			procCPUInfoPath, procCmdlinePath, unameRelease)
		hostFactsCached = true
	}
	facts := hostFactsStatic
	hostFactsMu.Unlock()
	// Re-probe the runtime-variable KVM signals every call: /dev/kvm may appear
	// after the cubelet started (on-demand kvm.ko load), so latching KVMAPIVersion
	// at the first collection's 0 would report kvm_api_version permanently "not
	// verified" and freeze a 0 onto any snapshot created in that window.
	facts.KVMAPIVersion = kvmAPIVersionFn()
	facts.KVMModuleFingerprint, facts.KVMModuleTaint, facts.KVMModuleScanned = collectKVMModuleState(sysModulePath)
	return facts
}

// staticFactsEmpty reports whether the boot-stable collection produced nothing
// usable — the signal that a first collection raced an unreadable source and
// should be retried rather than cached. KVMAPIVersion is deliberately excluded:
// it is re-probed per call, not part of the cached static set.
func staticFactsEmpty(f HostFacts) bool {
	return f.CPUVendor == "" && f.CPUModel == "" && f.CPUIDHash == "" &&
		f.HostKernelRelease == "" && f.HostKernelFingerprint == ""
}

// procCPUInfoPath, procCmdlinePath, sysModulePath and kvmAPIVersionFn are
// overridable in tests. kvmAPIVersionFn is a seam over the /dev/kvm probe so a
// test can exercise the per-call re-probe without a real device.
var (
	procCPUInfoPath = "/proc/cpuinfo"
	procCmdlinePath = "/proc/cmdline"
	sysModulePath   = "/sys/module"
	kvmAPIVersionFn = kvmAPIVersion
)

// collectStaticHostFacts reads the boot-stable inputs. Sources are injected so
// tests can exercise parsing and graceful degradation without touching the real
// host. The kernel fingerprint folds the release and the normalised boot cmdline
// (both boot-stable), so it is computed here rather than per call. KVMAPIVersion
// is NOT read here — it is runtime-variable and re-probed per call by
// CollectHostFacts.
func collectStaticHostFacts(
	cpuinfoPath, cmdlinePath string,
	kernelReleaseFn func() string,
) HostFacts {
	release := kernelReleaseFn()
	facts := HostFacts{
		HostKernelRelease:     release,
		HostKernelFingerprint: hashKernelIdentity(release, readCmdline(cmdlinePath)),
	}
	vendor, model, cpuidHash := parseCPUInfo(cpuinfoPath)
	facts.CPUVendor = vendor
	facts.CPUModel = model
	facts.CPUIDHash = cpuidHash
	return facts
}

// parseCPUInfo derives the CPU vendor, model name and a stable feature-set hash
// from /proc/cpuinfo. On x86 the hash folds vendor_id, cpu family, model,
// stepping and the (order-normalised) flag set; on ARM — where those x86 keys
// are absent — it folds the CPU implementer / architecture / variant / part /
// revision identity plus the Features list. Two hosts hash equal only when they
// present the same identity + feature surface. This is the strict-equality key
// used by the restore-compat judgment.
//
// The ARM identity keys are essential: without them two different ARM cores that
// happen to expose an identical Features list would hash equal, and since
// cpuid_hash is a *required* (blocking) dimension that would be a false
// "compatible=true".
//
// Assumption: the fleet is homogeneous per host — only the FIRST logical CPU
// block is hashed. On a heterogeneous (hybrid) host (big.LITTLE, Intel P+E
// cores, or a mixed-socket box) two machines that share the primary core but
// differ on secondary cores would hash equal, a false-positive "compatible".
// This is acceptable for the current uniform server fleet; revisit with a
// cross-block flag union if hybrid hosts enter the pool.
func parseCPUInfo(path string) (vendor, model, cpuidHash string) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()

	var id cpuIdentity
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, val, ok := splitCPUInfoLine(scanner.Text())
		if !ok {
			// A blank line separates the first logical CPU from the rest;
			// all cores are identical for our purposes, so stop after one.
			if strings.TrimSpace(scanner.Text()) == "" && (vendor != "" || id.armImplementer != "") {
				break
			}
			continue
		}
		switch key {
		case "vendor_id":
			if vendor == "" {
				vendor = val
			}
		case "model name":
			if model == "" {
				model = val
			}
		case "cpu family":
			setIfEmpty(&id.x86Family, val)
		case "model":
			setIfEmpty(&id.x86Model, val)
		case "stepping":
			setIfEmpty(&id.x86Stepping, val)
		case "CPU implementer":
			setIfEmpty(&id.armImplementer, val)
		case "CPU architecture":
			setIfEmpty(&id.armArch, val)
		case "CPU variant":
			setIfEmpty(&id.armVariant, val)
		case "CPU part":
			setIfEmpty(&id.armPart, val)
		case "CPU revision":
			setIfEmpty(&id.armRevision, val)
		case "flags", "Features":
			setIfEmpty(&id.flags, val)
		}
	}

	id.vendor = vendor
	cpuidHash = hashCPUFeatures(id)
	return vendor, model, cpuidHash
}

// cpuIdentity is the parsed per-arch identity surface folded into cpuid_hash.
type cpuIdentity struct {
	vendor      string
	x86Family   string
	x86Model    string
	x86Stepping string

	armImplementer string
	armArch        string
	armVariant     string
	armPart        string
	armRevision    string

	flags string
}

func setIfEmpty(dst *string, val string) {
	if *dst == "" {
		*dst = val
	}
}

func splitCPUInfoLine(line string) (key, val string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	val = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// hashCPUFeatures produces a deterministic hex digest of the CPU identity +
// feature set, covering both the x86 (vendor/family/model/stepping) and ARM
// (implementer/architecture/variant/part/revision) identity surfaces. Flags are
// sorted so kernel-ordering differences never change the hash. Returns "" when
// there is nothing meaningful to hash.
func hashCPUFeatures(id cpuIdentity) string {
	if id.vendor == "" && id.x86Family == "" && id.x86Model == "" &&
		id.armImplementer == "" && id.armPart == "" && id.flags == "" {
		return ""
	}
	sortedFlags := id.flags
	if id.flags != "" {
		parts := strings.Fields(id.flags)
		sort.Strings(parts)
		sortedFlags = strings.Join(parts, " ")
	}
	h := sha256.New()
	h.Write([]byte("vendor=" + id.vendor + "\n"))
	h.Write([]byte("family=" + id.x86Family + "\n"))
	h.Write([]byte("model=" + id.x86Model + "\n"))
	h.Write([]byte("stepping=" + id.x86Stepping + "\n"))
	h.Write([]byte("arm_implementer=" + id.armImplementer + "\n"))
	h.Write([]byte("arm_arch=" + id.armArch + "\n"))
	h.Write([]byte("arm_variant=" + id.armVariant + "\n"))
	h.Write([]byte("arm_part=" + id.armPart + "\n"))
	h.Write([]byte("arm_revision=" + id.armRevision + "\n"))
	h.Write([]byte("flags=" + sortedFlags + "\n"))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// cmdlineVolatileKeys are boot-parameter keys whose values differ per machine
// even when the two hosts are functionally identical (disk UUIDs, boot image
// paths, resume/dump targets). They are stripped before hashing so that
// same-configuration hosts fingerprint equal; the parameters that actually
// change kernel behaviour (mitigations, iommu, hugepages, nokaslr, isolcpus, …)
// are retained.
var cmdlineVolatileKeys = map[string]struct{}{
	"root":          {},
	"BOOT_IMAGE":    {},
	"resume":        {},
	"resume_offset": {},
	"crashkernel":   {},
	"initrd":        {},
}

// readCmdline returns the raw boot cmdline, or "" if unreadable.
func readCmdline(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// The kernel exposes cmdline NUL/newline-terminated; normalise whitespace.
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
}

// normaliseCmdline drops per-machine volatile keys and sorts the remaining
// parameters so that ordering differences never change the hash.
func normaliseCmdline(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	parts := strings.Fields(cmdline)
	kept := parts[:0]
	for _, p := range parts {
		key := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			key = p[:i]
		}
		if _, volatile := cmdlineVolatileKeys[key]; volatile {
			continue
		}
		kept = append(kept, p)
	}
	sort.Strings(kept)
	return strings.Join(kept, " ")
}

// hashKernelIdentity folds the kernel release and normalised boot cmdline into
// one deterministic digest. The global taint mask is intentionally excluded (see
// HostFacts doc). Returns "" when there is nothing to hash.
func hashKernelIdentity(release, cmdline string) string {
	normalised := normaliseCmdline(cmdline)
	if release == "" && normalised == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("release=" + release + "\n"))
	h.Write([]byte("cmdline=" + normalised + "\n"))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// suspiciousModuleTaintFlags are the per-module taint letters that indicate the
// module was not loaded through the normal signed-and-in-tree path and may have
// been tampered with:
//
//	E — module was loaded unsigned
//	F — module was force-loaded (version/CRC checks bypassed)
//	O — out-of-tree module (not part of the distributed kernel)
//	R — module was force-unloaded then reloaded
//	C — staging-tree module
//
// A KVM module carrying any of these is a hard restore blocker regardless of
// whether the origin node presented the same flag.
const suspiciousModuleTaintFlags = "EFORC"

// kvmModulePrefix matches the KVM module family: the core "kvm" module plus its
// per-arch / per-variant siblings (kvm_intel, kvm_amd, kvm_pvm, …). Names are
// normalised to underscores by the kernel in /sys/module.
const kvmModulePrefix = "kvm"

// collectKVMModuleState scans /sys/module for the KVM module family and returns
// two signals:
//
//   - fingerprint: a deterministic digest over each module's srcversion (build
//     checksum), initstate and code/init sizes — an equality key that changes if
//     any KVM module is swapped for a differently-built one.
//   - taint: the concatenation of suspicious per-module taint letters observed
//     across the family (sorted, deduplicated). Empty means every KVM module is
//     clean; non-empty is an absolute tamper indicator.
//
// The scanned return reports whether /sys/module was successfully read: it is
// true whenever the directory was listed (even when no KVM module is present),
// and false only when the read failed. This lets the caller tell a genuine
// "module unloaded" (scanned=true, empty fingerprint/taint — authoritative) from
// a transient read gap (scanned=false), so a stale taint can be cleared on a
// real unload instead of latched forever. Returns ("", "", false) when
// /sys/module is unreadable and ("", "", true) when it is readable but carries no
// KVM module.
func collectKVMModuleState(sysModuleDir string) (fingerprint, taint string, scanned bool) {
	entries, err := os.ReadDir(sysModuleDir)
	if err != nil {
		return "", "", false
	}
	names := make([]string, 0, 4)
	for _, e := range entries {
		name := e.Name()
		if name == kvmModulePrefix || strings.HasPrefix(name, kvmModulePrefix+"_") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", "", true
	}
	sort.Strings(names)

	h := sha256.New()
	taintSet := map[rune]struct{}{}
	for _, name := range names {
		dir := sysModuleDir + "/" + name
		src := readModuleField(dir, "srcversion")
		init := readModuleField(dir, "initstate")
		core := readModuleField(dir, "coresize")
		initsz := readModuleField(dir, "initsize")
		h.Write([]byte("module=" + name + "\n"))
		h.Write([]byte("srcversion=" + src + "\n"))
		h.Write([]byte("initstate=" + init + "\n"))
		h.Write([]byte("coresize=" + core + "\n"))
		h.Write([]byte("initsize=" + initsz + "\n"))

		for _, r := range readModuleField(dir, "taint") {
			if strings.ContainsRune(suspiciousModuleTaintFlags, r) {
				taintSet[r] = struct{}{}
			}
		}
	}

	flags := make([]rune, 0, len(taintSet))
	for r := range taintSet {
		flags = append(flags, r)
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i] < flags[j] })

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), string(flags), true
}

// readModuleField reads a single /sys/module/<name>/<field> value, trimmed.
// Returns "" when the field is absent (older kernels omit some attributes).
func readModuleField(moduleDir, field string) string {
	raw, err := os.ReadFile(moduleDir + "/" + field)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// unameRelease returns the running host kernel release (uname -r), or "" if the
// syscall fails.
func unameRelease() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return ""
	}
	return unix.ByteSliceToString(uts.Release[:])
}

// kvmAPIVersion opens /dev/kvm and issues KVM_GET_API_VERSION. Returns 0 when
// /dev/kvm is absent or the ioctl fails (e.g. running without virtualization),
// which the compatibility check treats as "unknown" rather than an error.
func kvmAPIVersion() int {
	fd, err := unix.Open(devKVMPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer unix.Close(fd)
	ver, err := unix.IoctlRetInt(fd, kvmGetAPIVersion)
	if err != nil {
		return 0
	}
	return ver
}
