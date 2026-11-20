// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cosapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// decodedPolicyParam undoes the double url-encoding sts.Client.sendRequest
// applies (it pre-escapes the Policy value before handing it to
// url.Values.Encode, which escapes again).
func decodedPolicyParam(t *testing.T, form url.Values, key string) string {
	t.Helper()
	once := form.Get(key)
	twice, err := url.QueryUnescape(once)
	if err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return twice
}

func TestIssueContentCredentials_ScopesPolicyToVolumePrefix(t *testing.T) {
	var capturedForm url.Values

	stsResponse := map[string]any{
		"Response": map[string]any{
			"Credentials": map[string]any{
				"TmpSecretId":  "tmp-id",
				"TmpSecretKey": "tmp-secret",
				"Token":        "tmp-token",
			},
			"ExpiredTime": time.Now().Add(15 * time.Minute).Unix(),
			"RequestId":   "req-1",
		},
	}
	body, err := json.Marshal(stsResponse)
	if err != nil {
		t.Fatalf("marshal stub response: %v", err)
	}

	c := &Client{
		bucket: "my-bucket-1250000000", region: "ap-guangzhou",
		secretID: "AKIDxxxx", secretKey: "secret", appID: "1250000000",
		stsHTTPCli: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "sts.tencentcloudapi.com" {
				t.Fatalf("unexpected STS host: %s", req.URL.Host)
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			form, err := url.ParseQuery(string(raw))
			if err != nil {
				t.Fatalf("parse request form: %v", err)
			}
			capturedForm = form
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}

	issued, err := c.IssueContentCredentials("vol-1")
	if err != nil {
		t.Fatalf("IssueContentCredentials: %v", err)
	}

	if got := capturedForm.Get("Action"); got != "GetFederationToken" {
		t.Fatalf("Action = %q, want GetFederationToken", got)
	}
	if got := capturedForm.Get("Region"); got != "ap-guangzhou" {
		t.Fatalf("Region = %q, want ap-guangzhou", got)
	}
	if got := capturedForm.Get("DurationSeconds"); got != fmt.Sprintf("%d", int64(maxContentCredentialsTTL.Seconds())) {
		t.Fatalf("DurationSeconds = %q, want %d seconds (CubeMaster's 15-minute ceiling)", got, int64(maxContentCredentialsTTL.Seconds()))
	}

	policy := decodedPolicyParam(t, capturedForm, "Policy")
	wantObjectResource := "qcs::cos:ap-guangzhou:uid/1250000000:my-bucket-1250000000/volumes/vol-1/*"
	if !strings.Contains(policy, wantObjectResource) {
		t.Fatalf("policy %s does not scope object actions to %s", policy, wantObjectResource)
	}
	wantBucketResource := "qcs::cos:ap-guangzhou:uid/1250000000:my-bucket-1250000000/"
	if !strings.Contains(policy, wantBucketResource) {
		t.Fatalf("policy %s does not scope list action to %s", policy, wantBucketResource)
	}
	if !strings.Contains(policy, "cos:prefix") || !strings.Contains(policy, "volumes/vol-1/*") {
		t.Fatalf("policy %s is missing the cos:prefix list condition", policy)
	}
	if strings.Contains(policy, "volumes/vol-2") {
		t.Fatalf("policy %s leaked another volume's prefix", policy)
	}

	if issued.Bucket != "my-bucket-1250000000" || issued.Region != "ap-guangzhou" {
		t.Fatalf("unexpected bucket/region in credentials: %+v", issued)
	}
	if issued.Prefix != "volumes/vol-1/" {
		t.Fatalf("Prefix = %q, want volumes/vol-1/", issued.Prefix)
	}
	if issued.Endpoint != "https://cos.ap-guangzhou.myqcloud.com" {
		t.Fatalf("Endpoint = %q", issued.Endpoint)
	}
	if issued.AddressingStyle != "virtual" {
		t.Fatalf("AddressingStyle = %q, want virtual", issued.AddressingStyle)
	}
	if issued.AccessKeyID != "tmp-id" || issued.SecretAccessKey != "tmp-secret" || issued.SessionToken != "tmp-token" {
		t.Fatalf("unexpected temporary credentials: %+v", issued)
	}
	if !issued.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt %v should be in the future", issued.ExpiresAt)
	}
}

func TestIssueContentCredentials_FailsFastWithoutResolvableAppID(t *testing.T) {
	c := &Client{bucket: "my-bucket-prod", region: "ap-guangzhou", secretID: "id", secretKey: "key"}
	if _, err := c.IssueContentCredentials("vol-1"); err == nil {
		t.Fatal("expected error when APPID cannot be resolved")
	}
}

func TestNew_DerivesAppIDFromBucketWithoutBreakingMissingAppID(t *testing.T) {
	cfg := &config.Config{
		SecretID: "id", SecretKey: "key", Bucket: "my-bucket-prod", Region: "ap-guangzhou",
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v (a non-numeric bucket suffix must not fail Create/Destroy/Attach/Detach)", err)
	}
	if c.appID != "" {
		t.Fatalf("appID = %q, want empty when it cannot be derived", c.appID)
	}
}
