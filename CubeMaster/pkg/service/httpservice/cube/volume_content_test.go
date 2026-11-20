// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
)

type fakeContentPlugin struct {
	name              string
	issuedVolumeID    string
	issuedPrivateData string
	issuedSubject     string
}

func (p *fakeContentPlugin) Name() string { return p.name }

func (p *fakeContentPlugin) IssueCredentials(_ context.Context, volumeID, privateData, subject string) (*plugin.DirectAccessCredentials, error) {
	p.issuedVolumeID = volumeID
	p.issuedPrivateData = privateData
	p.issuedSubject = subject
	return &plugin.DirectAccessCredentials{
		Provider: "cos", Endpoint: "https://bucket-1234567890.cos.ap-test.myqcloud.com",
		Bucket: "bucket-1234567890", Region: "ap-test", Prefix: "volumes/vol-content/",
		AccessKeyID: "temporary-id", SecretAccessKey: "temporary-secret", SessionToken: "temporary-session",
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

func TestVolumeContentCredentialIssuanceReturnsScopedStorageAccess(t *testing.T) {
	r, db, controller := newVolumeTestEngine(t)
	content := &fakeContentPlugin{name: controller.Name()}
	plugin.RegisterContent(content)
	t.Cleanup(func() { plugin.UnregisterForTest(content.Name()) })
	require.NoError(t, db.Create(&models.VolumeRecord{
		VolumeID: "vol-content", Name: "vol-content", Driver: controller.Name(),
		Token: "legacy-create-token", PrivateData: "plugin-private-state",
	}).Error)

	request := httptest.NewRequest(http.MethodPost, "/cube/volume/vol-content/content-token", nil)
	request.Header.Set("cube_user_id", "trusted-caller")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", response.Header().Get("Pragma"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "temporary-id", body["access_key_id"])
	assert.Equal(t, "temporary-session", body["session_token"])
	assert.Equal(t, "volumes/vol-content/", body["prefix"])
	assert.Equal(t, "auto", body["addressing_style"])
	assert.NotContains(t, response.Body.String(), "legacy-create-token")
	assert.Equal(t, "vol-content", content.issuedVolumeID)
	assert.Equal(t, "plugin-private-state", content.issuedPrivateData)
	assert.Equal(t, "trusted-caller", content.issuedSubject)
}

func TestValidDirectAccessCredentialsAcceptsS3CompatibleProviders(t *testing.T) {
	credentials := &plugin.DirectAccessCredentials{
		Provider: plugin.DirectAccessProviderS3, Endpoint: "https://s3.example.test",
		Bucket: "volume-bucket", Region: "test-region", Prefix: "volumes/vol-content/",
		AccessKeyID: "temporary-id", SecretAccessKey: "temporary-secret", SessionToken: "temporary-session",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	assert.True(t, validDirectAccessCredentials(credentials, "vol-content"))

	credentials.Provider = plugin.DirectAccessProviderCOS
	credentials.Endpoint = "https://bucket-1234567890.cos.ap-test.myqcloud.com"
	assert.True(t, validDirectAccessCredentials(credentials, "vol-content"))

	credentials.Provider = "unknown"
	assert.False(t, validDirectAccessCredentials(credentials, "vol-content"))
}

func TestValidDirectAccessCredentialsRejectsTTLAboveLimit(t *testing.T) {
	credentials := &plugin.DirectAccessCredentials{
		Provider: plugin.DirectAccessProviderS3, Endpoint: "https://s3.example.test",
		Bucket: "volume-bucket", Region: "test-region", Prefix: "volumes/vol-content/",
		AccessKeyID: "temporary-id", SecretAccessKey: "temporary-secret", SessionToken: "temporary-session",
		ExpiresAt: time.Now().Add(maxDirectAccessCredentialsTTL + time.Second),
	}
	assert.False(t, validDirectAccessCredentials(credentials, "vol-content"))

	credentials.ExpiresAt = time.Now().Add(maxDirectAccessCredentialsTTL - time.Second)
	assert.True(t, validDirectAccessCredentials(credentials, "vol-content"))
}

func TestVolumeContentCredentialRejectsUnscopedCredentials(t *testing.T) {
	r, db, controller := newVolumeTestEngine(t)
	content := &fakeContentPlugin{name: controller.Name()}
	plugin.RegisterContent(content)
	t.Cleanup(func() { plugin.UnregisterForTest(content.Name()) })
	require.NoError(t, db.Create(&models.VolumeRecord{
		VolumeID: "vol-content", Name: "vol-content", Driver: controller.Name(), PrivateData: "plugin-state",
	}).Error)

	// Replace the registered hook with a deliberately cross-volume scope.
	unscoped := &unscopedContentPlugin{fakeContentPlugin: content}
	plugin.UnregisterForTest(content.Name())
	plugin.RegisterContent(unscoped)

	request := httptest.NewRequest(http.MethodPost, "/cube/volume/vol-content/content-token", nil)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.NotContains(t, response.Body.String(), "temporary-secret")
	var body struct {
		Ret struct {
			RetCode int `json:"ret_code"`
		} `json:"ret"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), body.Ret.RetCode)
}

type unscopedContentPlugin struct{ *fakeContentPlugin }

func (p *unscopedContentPlugin) IssueCredentials(ctx context.Context, volumeID, privateData, subject string) (*plugin.DirectAccessCredentials, error) {
	credentials, err := p.fakeContentPlugin.IssueCredentials(ctx, volumeID, privateData, subject)
	if err == nil {
		credentials.Prefix = "volumes/another-volume/"
	}
	return credentials, err
}
