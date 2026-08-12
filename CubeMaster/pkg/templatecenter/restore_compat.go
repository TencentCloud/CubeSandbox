// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

// RestoreCompatReason enumerates the terminal verdicts that short-circuit a
// dimension-by-dimension comparison (no usable data on one side).
const (
	RestoreCompatReasonOriginFingerprintUnknown = "origin_fingerprint_unknown"
	RestoreCompatReasonTargetNodeUnknown        = "target_node_unknown"
)

// RestoreCompatDimension is a single strict-equality comparison between the
// snapshot's origin host (A) and the target node (B). Required=false means the
// dimension is informational and does not affect the overall verdict.
type RestoreCompatDimension struct {
	Name     string `json:"name"`
	Match    bool   `json:"match"`
	Required bool   `json:"required"`
	Origin   string `json:"origin"`
	Target   string `json:"target"`
}

// RestoreCompatResult is the control-plane judgment of whether a snapshot
// created on the origin node can be restored on the target node.
type RestoreCompatResult struct {
	Compatible bool                     `json:"compatible"`
	SnapshotID string                   `json:"snapshot_id"`
	OriginNode string                   `json:"origin_node,omitempty"`
	TargetNode string                   `json:"target_node"`
	Reason     string                   `json:"reason,omitempty"`
	Dimensions []RestoreCompatDimension `json:"dimensions,omitempty"`
}

// EvaluateSnapshotRestoreCompat judges whether snapshotID (created on its origin
// node A) can be restored on targetNode B. The v1 policy requires strict
// equality of only two dimensions — cpuid_hash and host_kernel_release — plus an
// absolute kvm_module_taint security gate. The richer facts (cpu_vendor /
// cpu_model / host_kernel_fingerprint / kvm_api_version / kvm_module_fingerprint)
// and the guest component versions are still reported, but only as informational
// (non-required) dimensions so they do not block restore yet; they exist for
// debugging and a future, tighter policy.
//
// The origin fingerprint is the one frozen into the snapshot at create time, so
// the judgment is immune to later host drift on A. When either side lacks usable
// data the result is Compatible=false with a Reason set.
func EvaluateSnapshotRestoreCompat(ctx context.Context, snapshotID, targetNode string) (*RestoreCompatResult, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	targetNode = strings.TrimSpace(targetNode)

	info, err := GetSnapshotInfo(ctx, snapshotID, false)
	if err != nil {
		return nil, err
	}

	result := &RestoreCompatResult{
		SnapshotID: snapshotID,
		OriginNode: info.OriginNodeID,
		TargetNode: targetNode,
	}

	origin := parseFrozenHostFacts(info.OriginHostFactsJSON)
	if origin == nil {
		result.Reason = RestoreCompatReasonOriginFingerprintUnknown
		return result, nil
	}

	target, ok := nodemeta.GetNodeHostFacts(ctx, targetNode)
	if !ok || target == nil {
		result.Reason = RestoreCompatReasonTargetNodeUnknown
		return result, nil
	}

	originGuestImage, originAgent := replicaGuestVersions(info)
	targetVersions, versionsOK := nodemeta.GetNodeComponentVersions(ctx, targetNode)
	if !versionsOK {
		targetVersions = nil
	}

	dims := buildHostFactDimensions(origin, target)
	dims = append(dims, buildGuestDimensions(originGuestImage, originAgent, targetVersions)...)
	result.Dimensions = dims
	result.Compatible = allRequiredDimensionsMatch(dims)
	return result, nil
}

// allRequiredDimensionsMatch returns true only when every required dimension
// matches. Informational (non-required) dimensions never affect the verdict.
func allRequiredDimensionsMatch(dims []RestoreCompatDimension) bool {
	for _, d := range dims {
		if d.Required && !d.Match {
			return false
		}
	}
	return true
}

// parseFrozenHostFacts decodes the origin host facts frozen onto the snapshot.
// Returns nil when absent (pre-upgrade snapshot) or unparseable.
func parseFrozenHostFacts(raw string) *nodemeta.HostFacts {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var facts nodemeta.HostFacts
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil
	}
	if facts.IsZero() {
		return nil
	}
	return &facts
}

// buildHostFactDimensions builds the strict-equality comparisons. The v1 policy
// makes only cpuid_hash and host_kernel_release required (blocking); the richer
// facts are collected and reported but demoted to informational so boot-param
// drift, rolling kvm.ko upgrades and marketing model strings do not split the
// node pool. The kvm_module_taint gate stays required — see below.
func buildHostFactDimensions(origin, target *nodemeta.HostFacts) []RestoreCompatDimension {
	dims := []RestoreCompatDimension{
		newDimension("cpuid_hash", true, origin.CPUIDHash, target.CPUIDHash),
		newDimension("host_kernel_release", true, origin.HostKernelRelease, target.HostKernelRelease),
		// Informational only: reported for debugging and a future tighter policy.
		newDimension("cpu_vendor", false, origin.CPUVendor, target.CPUVendor),
		newDimension("cpu_model", false, origin.CPUModel, target.CPUModel),
		// The folded fingerprint (release + normalised boot cmdline) is stricter
		// than the bare release; kept as a diagnostic signal only.
		kernelFingerprintDimension(origin, target),
		newDimension("kvm_api_version", false, kvmVersionString(origin.KVMAPIVersion), kvmVersionString(target.KVMAPIVersion)),
		// KVM module build identity (srcversion + initstate + sizes); a differently
		// built kvm.ko changes the restore ABI, but this is informational in v1.
		newDimension("kvm_module_fingerprint", false, origin.KVMModuleFingerprint, target.KVMModuleFingerprint),
	}
	dims = append(dims, kvmModuleTaintDimension(origin, target)...)
	return dims
}

// kvmModuleTaintDimension is an integrity gate on the *target* node. It is an
// absolute blocker only when the target's live KVM module carries a suspicious
// taint (unsigned / forced / out-of-tree / staging): a snapshot must never be
// restored onto a currently-tampered host, regardless of what the origin
// presented.
//
// The origin's own taint is deliberately NOT blocking. The origin facts are
// frozen once at snapshot-create time, so an origin taint is stale (the host may
// have been rebooted with a clean module since) and cannot be re-verified. Worse,
// making it blocking is self-defeating: it would permanently mark the snapshot
// incompatible with *every* node — including the origin itself, the only node
// RollbackSandboxToSnapshot actually permits — over a flag captured in a single
// heartbeat window. On fleets that legitimately run unsigned or out-of-tree
// kvm.ko (custom/vendored kernels), O and E are the norm and would otherwise
// yield a uniformly negative verdict. An origin-only taint is therefore reported
// as an informational (non-blocking) dimension so operators still see it.
//
// The dimension is omitted only when neither side reported any module taint
// signal, to avoid penalising pre-upgrade snapshots.
func kvmModuleTaintDimension(origin, target *nodemeta.HostFacts) []RestoreCompatDimension {
	if origin.KVMModuleTaint == "" && target.KVMModuleTaint == "" {
		return nil
	}
	// Blocking only when the live target is tainted; an origin-only taint is
	// stale/unverifiable and is reported informationally.
	return []RestoreCompatDimension{{
		Name:     "kvm_module_taint",
		Match:    false,
		Required: target.KVMModuleTaint != "",
		Origin:   origin.KVMModuleTaint,
		Target:   target.KVMModuleTaint,
	}}
}

// kernelFingerprintDimension compares the folded host-kernel digest. When either
// side lacks a fingerprint (e.g. a pre-upgrade snapshot frozen before the field
// existed, or an old cubelet target reporting release-only), both sides fall
// back to the bare kernel release so the comparison stays like-for-like —
// comparing a "sha256:…" digest against a "5.15.0" release could never match and
// would report a spurious mismatch during a mixed-version rolling upgrade. This
// dimension is informational only, so it never blocks; the fallback keeps the
// reported match/mismatch meaningful for diagnosis.
func kernelFingerprintDimension(origin, target *nodemeta.HostFacts) RestoreCompatDimension {
	o, t := origin.HostKernelFingerprint, target.HostKernelFingerprint
	if o == "" || t == "" {
		o, t = origin.HostKernelRelease, target.HostKernelRelease
	}
	return newDimension("host_kernel_fingerprint", false, o, t)
}

// buildGuestDimensions compares the guest-environment versions from the ready
// replica against the target node's currently reported versions. These are
// informational (Required=false): the guest kernel sha256 is already enforced
// strictly by the shim's SnapshotInfo::eq at restore time. When the target's
// versions are unknown the dimensions are omitted rather than reported as false.
func buildGuestDimensions(originGuestImage, originAgent string, targetVersions map[string]string) []RestoreCompatDimension {
	if originGuestImage == "" && originAgent == "" {
		return nil
	}
	if targetVersions == nil {
		return nil
	}
	dims := make([]RestoreCompatDimension, 0, 2)
	if originGuestImage != "" {
		dims = append(dims, newDimension("guest_image_version", false, originGuestImage, targetVersions["guest-image"]))
	}
	if originAgent != "" {
		dims = append(dims, newDimension("cube_agent_version", false, originAgent, targetVersions["cube-agent"]))
	}
	return dims
}

// replicaGuestVersions returns the guest-image and cube-agent versions recorded
// on the snapshot's replica (all replicas of one snapshot share these).
func replicaGuestVersions(info *SnapshotInfo) (guestImage, agent string) {
	for _, r := range info.Replicas {
		if guestImage == "" {
			guestImage = strings.TrimSpace(r.GuestImageVersion)
		}
		if agent == "" {
			agent = strings.TrimSpace(r.AgentVersion)
		}
		if guestImage != "" && agent != "" {
			break
		}
	}
	return guestImage, agent
}

// newDimension builds one comparison. A dimension matches only when BOTH sides
// carry a non-empty value that is equal: two empty values are treated as "not
// verified", not a match. This keeps required dimensions failing closed when a
// signal is missing on both sides (e.g. neither cubelet could open /dev/kvm or
// read /sys/module), instead of passing vacuously ("" == ""), and keeps the
// informational match:true flag honest — it always means "verified equal".
func newDimension(name string, required bool, origin, target string) RestoreCompatDimension {
	origin = strings.TrimSpace(origin)
	target = strings.TrimSpace(target)
	return RestoreCompatDimension{
		Name:     name,
		Match:    origin != "" && origin == target,
		Required: required,
		Origin:   origin,
		Target:   target,
	}
}

func kvmVersionString(v int) string {
	if v == 0 {
		return ""
	}
	return strconv.Itoa(v)
}
