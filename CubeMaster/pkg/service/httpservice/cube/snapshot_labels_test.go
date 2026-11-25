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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

// snapshotRequest is what a snapshot records: the source sandbox's create
// request, its user metadata and Cube's own labels included.
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

// createRequest builds the request the way the server does for POST /cube/sandbox.
func createRequest(t *testing.T, labels string) *types.CreateCubeSandboxReq {
	t.Helper()
	body := `{"requestID":"req-1","labels":` + labels + `}`
	req, err := constructCreateReq(httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(body)))
	require.NoError(t, err)
	return req
}

func userMetadataOf(labels map[string]string) map[string]string {
	return webhook.UserMetadata(labels).Metadata
}

func TestSnapshotCreateKeepsTheRequestsMetadata(t *testing.T) {
	request := createRequest(t, `{"name":"copy","purpose":"review"}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, map[string]string{"name": "copy", "purpose": "review"}, userMetadataOf(request.Labels))
	require.Equal(t, "nightly", request.Labels[constants.CubeAnnotationAppSnapshotTemplateAlias])
}

func TestSnapshotCreateWithoutMetadataInheritsTheSources(t *testing.T) {
	request := createRequest(t, `{}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, map[string]string{
		"name":             "source-sandbox",
		"owner":            "alice",
		"com.example.team": "core",
		"workload":         "burst_balance",
	}, userMetadataOf(request.Labels))
}

func TestSnapshotCreateCannotOverrideCubesOwnLabels(t *testing.T) {
	request := createRequest(t, `{"name":"copy","`+constants.CubeAnnotationComponentEnvdVersion+`":"spoofed"}`)

	applySnapshotTemplateAnnotationsAndLabels(snapshotRequest(), request)

	require.Equal(t, "1.2.3", request.Labels[constants.CubeAnnotationComponentEnvdVersion])
	require.Equal(t, map[string]string{"name": "copy"}, userMetadataOf(request.Labels))
}
