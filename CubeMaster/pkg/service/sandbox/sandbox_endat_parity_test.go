package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// Parity between the detail and list paths for endAt / endAtStatus.
// Both must derive the tri-state from the same live metadata read and must
// agree with each other and with the endAt they report.

func TestEndAtStatusNeverBeatsStaleItemEndAt(t *testing.T) {
	// The live metadata says never. A stale positive endAt on the item (read
	// moments earlier, before set-timeout(-1)) must not turn it into deadline.
	never := types.NeverTimeout
	got := endAtStatusFor(1_700_000_000_000,
		DeadlineInfo{Found: true, TimeoutSeconds: &never})
	if got != types.EndAtStatusNever {
		t.Fatalf("status: got %q, want never", got)
	}
}

func TestApplySandboxConfigFillsEndAtFromTheSameLiveRead(t *testing.T) {
	finite := 600
	items := []*types.SandboxData{{SandboxID: "sb"}}
	specs := map[string]*types.CreateCubeSandboxReq{"sb": {}}
	deadlines := map[string]DeadlineInfo{
		"sb": {EndAtMs: 1_800_000_000_000, TimeoutSeconds: &finite, Found: true},
	}

	applySandboxConfig(items, specs, deadlines)

	if items[0].EndAt != 1_800_000_000_000 {
		t.Fatalf("endAt: got %d, want the live deadline", items[0].EndAt)
	}
	if items[0].EndAtStatus != types.EndAtStatusDeadline {
		t.Fatalf("status: got %q, want deadline", items[0].EndAtStatus)
	}
}

func TestApplySandboxConfigNeverClearsStaleEndAt(t *testing.T) {
	never := types.NeverTimeout
	items := []*types.SandboxData{{SandboxID: "sb", EndAt: 1_700_000_000_000}}
	specs := map[string]*types.CreateCubeSandboxReq{"sb": {}}
	deadlines := map[string]DeadlineInfo{
		"sb": {TimeoutSeconds: &never, Found: true},
	}

	applySandboxConfig(items, specs, deadlines)

	if items[0].EndAtStatus != types.EndAtStatusNever {
		t.Fatalf("status: got %q, want never", items[0].EndAtStatus)
	}
	if items[0].EndAt != 0 {
		t.Fatalf("endAt: got %d, want 0 for a never-timeout sandbox", items[0].EndAt)
	}
}

func TestApplySandboxConfigStatusDoesNotDependOnSpecRead(t *testing.T) {
	// The tri-state comes from the live metadata, not from the spec row; an
	// unreadable spec must not blank it (list always reports the tri-state).
	never := types.NeverTimeout
	items := []*types.SandboxData{{SandboxID: "sb"}}
	deadlines := map[string]DeadlineInfo{
		"sb": {TimeoutSeconds: &never, Found: true},
	}

	applySandboxConfig(items, nil, deadlines)

	if items[0].EndAtStatus != types.EndAtStatusNever {
		t.Fatalf("status: got %q, want never even without a spec", items[0].EndAtStatus)
	}
	if items[0].AutoPause != nil || items[0].CubeNetworkConfig != nil {
		t.Fatalf("spec-derived fields must stay empty: %+v", items[0])
	}
}
