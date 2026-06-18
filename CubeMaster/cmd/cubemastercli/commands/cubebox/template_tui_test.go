// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:                    "512B",
		1024:                   "1.0KiB",
		5 * 1024 * 1024:        "5.0MiB",
		3 * 1024 * 1024 * 1024: "3.0GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d)=%q want %q", in, got, want)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	if got := formatElapsed(75 * time.Second); got != "01:15" {
		t.Fatalf("formatElapsed=%q", got)
	}
}

func TestClampUnit(t *testing.T) {
	if clampUnit(-1) != 0 || clampUnit(2) != 1 || clampUnit(0.5) != 0.5 {
		t.Fatalf("clampUnit out of range")
	}
}

func TestShouldUseTUIRespectsJSONAndDetach(t *testing.T) {
	jsonCtx := newCreateFromImageContext(t, []string{"--json"})
	if shouldUseTUI(jsonCtx) {
		t.Fatalf("--json must disable TUI")
	}
	detachCtx := newCreateFromImageContext(t, []string{"--detach"})
	if shouldUseTUI(detachCtx) {
		t.Fatalf("--detach must disable TUI")
	}
	noWaitCtx := newCreateFromImageContext(t, []string{"--no-wait"})
	if !detachRequested(noWaitCtx) {
		t.Fatalf("--no-wait must alias --detach")
	}
}

func TestImageJobTUITerminalTransitions(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)

	// READY -> done, no fatal.
	m := newImageJobTUI(ctx, "job-1")
	updated, cmd := m.Update(imageJobMsg{job: &types.TemplateImageJobInfo{Status: "READY", Progress: 100}})
	model := updated.(imageJobTUI)
	if !model.done || model.fatal != nil {
		t.Fatalf("READY should finish without fatal: done=%v fatal=%v", model.done, model.fatal)
	}
	if cmd == nil {
		t.Fatalf("expected quit cmd on READY")
	}

	// FAILED -> done with fatal carrying the error message.
	m = newImageJobTUI(ctx, "job-1")
	updated, _ = m.Update(imageJobMsg{job: &types.TemplateImageJobInfo{Status: "FAILED", ErrorMessage: "boom"}})
	model = updated.(imageJobTUI)
	if !model.done || model.fatal == nil || model.fatal.Error() != "boom" {
		t.Fatalf("FAILED should set fatal: %+v", model.fatal)
	}

	// RUNNING -> keep polling, not done.
	m = newImageJobTUI(ctx, "job-1")
	updated, cmd = m.Update(imageJobMsg{job: &types.TemplateImageJobInfo{Status: "RUNNING", Phase: "PULLING", Progress: 5}})
	model = updated.(imageJobTUI)
	if model.done {
		t.Fatalf("RUNNING must not be terminal")
	}
	if cmd == nil {
		t.Fatalf("expected next poll tick cmd")
	}
}

func TestImageJobTUITransientErrorKeepsPolling(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	m := newImageJobTUI(ctx, "job-1")
	updated, cmd := m.Update(imageJobMsg{err: errString("temporary")})
	model := updated.(imageJobTUI)
	if model.done {
		t.Fatalf("transient error must not finish watch")
	}
	if model.pollErr == nil {
		t.Fatalf("pollErr should be recorded")
	}
	if cmd == nil {
		t.Fatalf("expected retry tick cmd")
	}
}

func TestImageJobWatchUnexpectedModelFallsBack(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	fallbackErr := errString("fallback used")
	err := finishImageJobWatch(ctx, "job-1", fakeTeaModel{}, func(*cli.Context, string) error {
		return fallbackErr
	})
	if err != fallbackErr {
		t.Fatalf("finishImageJobWatch error=%v want fallback error", err)
	}
}

func TestImageJobStepsRendering(t *testing.T) {
	if got := stepIndexForPhase("DISTRIBUTING"); got != 4 {
		t.Fatalf("DISTRIBUTING index=%d want 4", got)
	}
	if got := stepIndexForPhase("BOGUS"); got != -1 {
		t.Fatalf("unknown phase should be -1, got %d", got)
	}

	ctx := newCreateFromImageContext(t, nil)
	m := newImageJobTUI(ctx, "job-1")
	updated, _ := m.Update(imageJobMsg{job: &types.TemplateImageJobInfo{Status: "RUNNING", Phase: "DISTRIBUTING"}})
	model := updated.(imageJobTUI)
	if got := model.currentStepIndex(); got != 4 {
		t.Fatalf("currentStepIndex=%d want 4", got)
	}
	// An unknown later phase must not snap the checklist backwards.
	updated, _ = model.Update(imageJobMsg{job: &types.TemplateImageJobInfo{Status: "RUNNING", Phase: "BOGUS"}})
	model = updated.(imageJobTUI)
	if got := model.currentStepIndex(); got != 4 {
		t.Fatalf("high-water step regressed to %d", got)
	}
	view := model.renderSteps()
	if !strings.Contains(view, "PULLING") || !strings.Contains(view, "CREATING_TEMPLATE") {
		t.Fatalf("steps view missing entries: %q", view)
	}
}

func TestImageJobPullRenderingBytes(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	m := newImageJobTUI(ctx, "job-1")
	m.job = &types.TemplateImageJobInfo{
		Status: "RUNNING", Phase: "PULLING",
		PullTotalBytes: 10 * 1024 * 1024, PullDownloadedBytes: 5 * 1024 * 1024,
		PullSpeedBPS: 1024 * 1024, PullTotalLayers: 3, PullCompletedLayers: 1,
	}
	out := m.renderPull()
	if !strings.Contains(out, "MiB") || !strings.Contains(out, "layer 1/3") {
		t.Fatalf("pull render missing details: %q", out)
	}
}

func TestBuildJobTUITerminalTransitions(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)

	m := newBuildJobTUI(ctx, "build-1")
	updated, cmd := m.Update(buildStatusMsg{rsp: &templateBuildStatusResponse{Status: "ready", Progress: 100}})
	model := updated.(buildJobTUI)
	if !model.done || model.fatal != nil {
		t.Fatalf("ready should finish without fatal: done=%v fatal=%v", model.done, model.fatal)
	}
	if cmd == nil {
		t.Fatalf("expected quit cmd on ready")
	}

	m = newBuildJobTUI(ctx, "build-1")
	updated, _ = m.Update(buildStatusMsg{rsp: &templateBuildStatusResponse{Status: "error", Message: "boom"}})
	model = updated.(buildJobTUI)
	if !model.done || model.fatal == nil || model.fatal.Error() != "boom" {
		t.Fatalf("error should set fatal: %+v", model.fatal)
	}

	m = newBuildJobTUI(ctx, "build-1")
	updated, _ = m.Update(buildStatusMsg{rsp: &templateBuildStatusResponse{Status: "error"}})
	model = updated.(buildJobTUI)
	if model.fatal == nil || model.fatal.Error() != "sandbox commit build failed" {
		t.Fatalf("empty error message fatal=%v", model.fatal)
	}

	m = newBuildJobTUI(ctx, "build-1")
	updated, cmd = m.Update(buildStatusMsg{rsp: &templateBuildStatusResponse{Status: "building", Progress: 5}})
	model = updated.(buildJobTUI)
	if model.done {
		t.Fatalf("building must not be terminal")
	}
	if cmd == nil {
		t.Fatalf("expected next poll tick cmd")
	}
}

func TestBuildJobTUITransientErrorKeepsPolling(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	m := newBuildJobTUI(ctx, "build-1")
	updated, cmd := m.Update(buildStatusMsg{err: errString("temporary")})
	model := updated.(buildJobTUI)
	if model.done {
		t.Fatalf("transient error must not finish watch")
	}
	if model.pollErr == nil {
		t.Fatalf("pollErr should be recorded")
	}
	if cmd == nil {
		t.Fatalf("expected retry tick cmd")
	}
}

func TestBuildJobTUICancel(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	m := newBuildJobTUI(ctx, "build-1")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	model := updated.(buildJobTUI)
	if !model.done || !model.canceled {
		t.Fatalf("q should cancel watch: done=%v canceled=%v", model.done, model.canceled)
	}
	if cmd == nil {
		t.Fatalf("expected quit cmd on cancel")
	}
}

func TestBuildWatchUnexpectedModelFallsBack(t *testing.T) {
	ctx := newCreateFromImageContext(t, nil)
	fallbackErr := errString("fallback used")
	err := finishBuildWatch(ctx, "build-1", fakeTeaModel{}, func(*cli.Context, string) error {
		return fallbackErr
	})
	if err != fallbackErr {
		t.Fatalf("finishBuildWatch error=%v want fallback error", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

type fakeTeaModel struct{}

func (fakeTeaModel) Init() tea.Cmd { return nil }

func (fakeTeaModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return fakeTeaModel{}, nil }

func (fakeTeaModel) View() string { return "" }
