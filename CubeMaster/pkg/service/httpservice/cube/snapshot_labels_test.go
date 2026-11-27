// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func snapshotRequest() *types.CreateCubeSandboxReq {
	return &types.CreateCubeSandboxReq{
		Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateAlias: "nightly"},
		Labels: map[string]string{
			"name":             "source-sandbox",
			"owner":            "alice",
			"com.example.team": "core",
			"workload":         "burst_balance",
			constants.Caller:   "source-caller",
			constants.CubeAnnotationComponentEnvdVersion: "1.2.3",
		},
	}
}

func createRequest(t *testing.T, labels string) *types.CreateCubeSandboxReq {
	t.Helper()
	body := `{"requestID":"req-1","labels":` + labels + `}`
	req, err := constructCreateReq(httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(body)))
	require.NoError(t, err)
	return req
}

func nonCubeLabelsOf(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if !strings.HasPrefix(k, "cube.master.") {
			out[k] = v
		}
	}
	return out
}

func TestSnapshotCreateRequestLabelsWin(t *testing.T) {
	request := createRequest(t, `{"name":"copy","purpose":"review"}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, map[string]string{
		"name":             "copy",
		"purpose":          "review",
		"owner":            "alice",
		"com.example.team": "core",
		"workload":         "burst_balance",
		constants.Caller:   "source-caller",
	}, nonCubeLabelsOf(request.Labels))
	require.Equal(t, "nightly", request.Labels[constants.CubeAnnotationAppSnapshotTemplateAlias])
}

func TestSnapshotCreateWithoutRequestLabelsInherits(t *testing.T) {
	request := createRequest(t, `{}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, map[string]string{
		"name":             "source-sandbox",
		"owner":            "alice",
		"com.example.team": "core",
		"workload":         "burst_balance",
		constants.Caller:   "source-caller",
	}, nonCubeLabelsOf(request.Labels))
}

func TestSnapshotCreateCannotOverrideCubeMasterLabels(t *testing.T) {
	request := createRequest(t, `{"name":"copy","`+constants.CubeAnnotationComponentEnvdVersion+`":"spoofed"}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, "1.2.3", request.Labels[constants.CubeAnnotationComponentEnvdVersion])
}
