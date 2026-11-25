// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// Adoption must reuse the READY artifact while regenerating the create request
// from the adopting template's own job request.
func TestRegisterRemoteBuiltArtifactAdoptsReadyArtifactWithOwnResources(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.TemplateImageJob{}, &models.TemplateReplica{}, &models.SnapshotRecord{}))

	const (
		fingerprint = "fp-shared-adopt"
		artifactID  = "rfs-shared-adopt"
	)
	// The artifact row carries the first builder's request (1C/1G).
	firstBuilderReq, err := json.Marshal(&sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Containers:   []*sandboxtypes.Container{{Resources: &sandboxtypes.Resource{Cpu: "1000m", Mem: "1024Mi"}}},
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: fingerprint,
		Status:                  ArtifactStatusReady,
		Ext4SHA256:              "deadbeef",
		Ext4SizeBytes:           1024,
		DownloadToken:           "tok",
		MasterNodeIP:            "http://cube-master:8089",
		GeneratedRequestJSON:    string(firstBuilderReq),
		ImageConfigJSON:         `{"Entrypoint":["/bin/sh"]}`,
	}).Error)

	// The adopting template asks for 2C/2G.
	req := &sandboxtypes.CreateTemplateFromImageReq{
		Request:           &sandboxtypes.Request{RequestID: "req-adopt-test"},
		TemplateID:        "tpl-2c2g",
		InstanceType:      "cubebox",
		WritableLayerSize: "1Gi",
		ContainerOverrides: &sandboxtypes.ContainerOverrides{
			Resources: &sandboxtypes.Resource{Cpu: "2000m", Mem: "2048Mi"},
		},
	}
	record, generatedReq, adopted, err := registerRemoteBuiltArtifactOnSession(
		context.Background(), db, req,
		&RemoteBuildResult{ArtifactID: artifactID, TemplateSpecFingerprint: fingerprint},
		DockerImageConfig{},
	)
	require.NoError(t, err)
	require.True(t, adopted, "READY row with the same fingerprint must be adopted")
	require.NotNil(t, record)
	require.Equal(t, artifactID, record.ArtifactID, "adoption must keep the artifact id")
	require.NotNil(t, generatedReq)
	require.Equal(t, "cubebox", generatedReq.InstanceType)
	require.Equal(t, "cpu=2000m,mem=2048Mi", calculateRequestSpec(generatedReq),
		"adopted template must use its own resources")
}
