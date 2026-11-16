// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cosapi

import (
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
	sts "github.com/tencentyun/qcloud-cos-sts-sdk/go"
)

// maxContentCredentialsTTL mirrors CubeMaster's hard ceiling
// (CubeMaster/pkg/service/httpservice/cube/volume_content.go:
// maxDirectAccessCredentialsTTL). Requesting more from STS would only get
// rejected by CubeMaster, so the Hook always asks for exactly this much.
const maxContentCredentialsTTL = 15 * time.Minute

// objectActions covers simple upload, multipart upload, download, stat and
// delete for a single object key. See the COS CAM action reference:
// https://cloud.tencent.com/document/product/436/31923
var objectActions = []string{
	"name/cos:GetObject",
	"name/cos:HeadObject",
	"name/cos:PutObject",
	"name/cos:PostObject",
	"name/cos:DeleteObject",
	"name/cos:InitiateMultipartUpload",
	"name/cos:ListMultipartUploads",
	"name/cos:ListParts",
	"name/cos:UploadPart",
	"name/cos:CompleteMultipartUpload",
	"name/cos:AbortMultipartUpload",
}

// listActions covers ListObjects (cos:GetBucket), which CubeSandbox's
// VolumeContent.list()/exists() need. ListObjects is a bucket-level API, so
// it is scoped back down to the volume prefix via a `cos:prefix` condition
// instead of the object-level resource ARN used by objectActions.
// https://cloud.tencent.com/document/product/436/71306
var listActions = []string{
	"name/cos:GetBucket",
}

// ContentCredentials are the short-lived, volume-scoped credentials returned
// by IssueContentCredentials. Field names mirror plugin.DirectAccessCredentials
// in CubeMaster (which this example does not import, to keep the rpc plugin
// module's dependency graph limited to the proto client).
type ContentCredentials struct {
	Endpoint        string
	Bucket          string
	Region          string
	Prefix          string
	AddressingStyle string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	ExpiresAt       time.Time
}

// IssueContentCredentials calls the COS STS GetFederationToken API
// (https://cloud.tencent.com/document/product/436/14048) and returns
// credentials limited to the actions above and the single volume's object
// prefix. The caller's SecretID/SecretKey need the CAM permission
// sts:GetFederationToken (granted by default to the account's root/admin
// identity; use a scoped sub-account in production).
func (c *Client) IssueContentCredentials(volumeID string) (*ContentCredentials, error) {
	if c.appID == "" {
		return nil, errors.New("cosapi: APPID is not configured and could not be derived from BUCKET; set APPID in volume-cos.conf")
	}
	prefix := config.VolumePrefix(volumeID)
	objectResource := fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s*", c.region, c.appID, c.bucket, prefix)
	bucketResource := fmt.Sprintf("qcs::cos:%s:uid/%s:%s/", c.region, c.appID, c.bucket)

	policy := &sts.CredentialPolicy{
		Statement: []sts.CredentialPolicyStatement{
			{
				Effect:   "allow",
				Action:   objectActions,
				Resource: []string{objectResource},
			},
			{
				Effect:   "allow",
				Action:   listActions,
				Resource: []string{bucketResource},
				Condition: map[string]map[string]interface{}{
					"string_like": {"cos:prefix": []string{prefix + "*"}},
				},
			},
		},
	}

	client := sts.NewClient(c.secretID, c.secretKey, c.stsHTTPCli)
	result, err := client.GetCredential(&sts.CredentialOptions{
		Region:          c.region,
		DurationSeconds: int64(maxContentCredentialsTTL.Seconds()),
		Policy:          policy,
	})
	if err != nil {
		return nil, fmt.Errorf("cos sts GetFederationToken: %w", err)
	}
	if result == nil || result.Credentials == nil {
		return nil, errors.New("cos sts GetFederationToken: empty credential response")
	}

	return &ContentCredentials{
		// Region endpoint (no bucket segment): boto3 prepends the bucket as
		// a subdomain when addressing_style is "virtual", which COS supports.
		Endpoint:        fmt.Sprintf("https://cos.%s.myqcloud.com", c.region),
		Bucket:          c.bucket,
		Region:          c.region,
		Prefix:          prefix,
		AddressingStyle: "virtual",
		AccessKeyID:     result.Credentials.TmpSecretID,
		SecretAccessKey: result.Credentials.TmpSecretKey,
		SessionToken:    result.Credentials.SessionToken,
		ExpiresAt:       time.Unix(int64(result.ExpiredTime), 0),
	}, nil
}
