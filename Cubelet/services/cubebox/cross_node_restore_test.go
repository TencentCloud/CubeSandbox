// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestExistingXFSS3SandboxIsNeverStartedTwice(t *testing.T) {
	const snap = "snap-replay0000000000000000001"
	now := time.Now().UnixNano()
	// A restart policy or an opaque restore clears the lineage of the very
	// instance a lost attempt started: it is still this sandbox.
	running := newCubeboxWithStatusForTest("sb-x", cubeboxstore.Status{StartedAt: now})
	running.IP = "192.168.0.7"
	paused := newCubeboxWithStatusForTest("sb-x", cubeboxstore.Status{PausedAt: now})
	exited := newCubeboxWithStatusForTest("sb-x", cubeboxstore.Status{StartedAt: now, FinishedAt: now})
	creating := newCubeboxWithStatusForTest("sb-x", cubeboxstore.Status{CreatedAt: now})

	req := func(backend string) *cubebox.RunCubeSandboxRequest {
		return &cubebox.RunCubeSandboxRequest{Annotations: map[string]string{
			constants.MasterAnnotationStorageBackend:  backend,
			constants.MasterAnnotationPauseSnapshotID: snap,
		}}
	}
	cases := []struct {
		name  string
		boxes []*cubeboxstore.CubeBox
		req   *cubebox.RunCubeSandboxRequest
		id    string
		want  xfss3ResumeRetry
	}{
		{"running without lineage is replayed", []*cubeboxstore.CubeBox{running}, req("xfs-s3"), "sb-x", xfss3ResumeReplay},
		{"paused tombstone resumes as usual", []*cubeboxstore.CubeBox{paused}, req("xfs-s3"), "sb-x", xfss3ResumeFresh},
		{"exited instance blocks a new start", []*cubeboxstore.CubeBox{exited}, req("xfs-s3"), "sb-x", xfss3ResumeBusy},
		{"creating instance blocks a new start", []*cubeboxstore.CubeBox{creating}, req("xfs-s3"), "sb-x", xfss3ResumeBusy},
		{"no record starts fresh", nil, req("xfs-s3"), "sb-x", xfss3ResumeFresh},
		{"another id does not match", []*cubeboxstore.CubeBox{running}, req("xfs-s3"), "sb-y", xfss3ResumeFresh},
		{"other backends keep their semantics", []*cubeboxstore.CubeBox{running}, req("xfs"), "sb-x", xfss3ResumeFresh},
	}
	for _, c := range cases {
		sb, got := existingXFSS3Sandbox(c.boxes, c.req, c.id)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if got == xfss3ResumeReplay && sb != running {
			t.Errorf("%s: replay must answer with the running sandbox", c.name)
		}
	}
}
