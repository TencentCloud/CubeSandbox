// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

const maxDirectAccessCredentialsTTL = 15 * time.Minute

// DirectAccessCredentialsItem is the management-plane response containing
// short-lived, volume-scoped storage credentials. Responses are never cacheable.
type DirectAccessCredentialsItem struct {
	Ret             *types.Ret `json:"ret,omitempty"`
	Provider        string     `json:"provider"`
	Endpoint        string     `json:"endpoint"`
	Bucket          string     `json:"bucket"`
	Region          string     `json:"region"`
	Prefix          string     `json:"prefix"`
	AddressingStyle string     `json:"addressing_style"`
	AccessKeyID     string     `json:"access_key_id"`
	SecretAccessKey string     `json:"secret_access_key"`
	SessionToken    string     `json:"session_token"`
	ExpiresAt       int64      `json:"expires_at"`
}

// handleIssueVolumeContentToken implements POST /cube/volume/{id}/content-token.
// It vends direct-storage credentials; file bytes are not proxied by CubeMaster.
func handleIssueVolumeContentToken(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	volumeID := c.Param("volume_id")
	if !volumeIDPattern.MatchString(volumeID) {
		common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "invalid volume id")})
		return
	}
	var record models.VolumeRecord
	err := volumeDB().WithContext(c.Request.Context()).Where("volume_id = ?", volumeID).First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_NotFound, "volume not found: "+volumeID)})
			return
		}
		common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+err.Error())})
		return
	}
	credentialPlugin, ok := plugin.GetContent(record.Driver)
	if !ok {
		common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "volume driver has no direct-access credential hook: "+record.Driver)})
		return
	}
	issued, err := credentialPlugin.IssueCredentials(c.Request.Context(), record.VolumeID, record.PrivateData, c.GetHeader(constants.AuthUserID))
	if err != nil {
		common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_MasterUnavailable, "storage credential issuer is unavailable: "+err.Error())})
		return
	}
	if !validDirectAccessCredentials(issued, volumeID) {
		common.WriteAPI(c, &DirectAccessCredentialsItem{Ret: volErr(rt, errorcode.ErrorCode_MasterInternalError, "credential hook returned invalid or unscoped credentials")})
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	rt.RetCode = 0
	addressingStyle := issued.AddressingStyle
	if addressingStyle == "" {
		addressingStyle = "auto"
	}
	common.WriteAPI(c, &DirectAccessCredentialsItem{
		Ret: retOK(), Provider: issued.Provider, Endpoint: issued.Endpoint, Bucket: issued.Bucket,
		Region: issued.Region, Prefix: issued.Prefix, AddressingStyle: addressingStyle,
		AccessKeyID: issued.AccessKeyID, SecretAccessKey: issued.SecretAccessKey,
		SessionToken: issued.SessionToken, ExpiresAt: issued.ExpiresAt.Unix(),
	})
}

func validDirectAccessCredentials(credentials *plugin.DirectAccessCredentials, volumeID string) bool {
	now := time.Now()
	if credentials == nil || !supportedDirectAccessProvider(credentials.Provider) || credentials.Bucket == "" || credentials.Region == "" ||
		credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" ||
		!credentials.ExpiresAt.After(now) || credentials.ExpiresAt.After(now.Add(maxDirectAccessCredentialsTTL)) ||
		!supportedS3AddressingStyle(credentials.AddressingStyle) {
		return false
	}
	parsed, err := url.Parse(credentials.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	prefix := "volumes/" + volumeID + "/"
	return credentials.Prefix == prefix && !strings.Contains(credentials.Prefix, "..")
}

func supportedDirectAccessProvider(provider string) bool {
	return provider == plugin.DirectAccessProviderS3 || provider == plugin.DirectAccessProviderCOS
}

func supportedS3AddressingStyle(style string) bool {
	return style == "" || style == "auto" || style == "path" || style == "virtual"
}

var volumeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
