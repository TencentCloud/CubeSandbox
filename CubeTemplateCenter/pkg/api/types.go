// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package api

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// BuildJobRequest is the payload sent by CubeMaster to submit a build job.
type BuildJobRequest struct {
	JobID           string                            `json:"job_id" binding:"required"`
	Request         *types.CreateTemplateFromImageReq `json:"request" binding:"required"`
	DownloadBaseURL string                            `json:"download_base_url"`
	EnvdSHA256      string                            `json:"envd_sha256"`
	EnvdData        []byte                            `json:"envd_data"`
}

// BuildJobResponse is the response returned to CubeMaster after accepting a build job.
type BuildJobResponse struct {
	Status string `json:"status"`
	JobID  string `json:"job_id"`
}

// ErrorResponse is a generic error response.
type ErrorResponse struct {
	Error string `json:"error"`
}
