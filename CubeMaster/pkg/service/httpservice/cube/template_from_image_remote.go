// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

// remoteTemplateBuildEnabled always returns true: CubeMaster no longer builds
// templates in-process, so every template-from-image build is forwarded to
// the standalone CubeTemplateCenter process.
//
// The templatecenter_enabled switch has been removed. There is no local
// fallback: a missing CUBE_TEMPLATE_CENTER_ADDR fails the build with a clear
// error instead of falling back to an in-process build.
func remoteTemplateBuildEnabled() bool {
	return true
}

// forwardBuildJobToTemplateCenter pushes an already-persisted job to
// CubeTemplateCenter. Runs in a background goroutine; any transport failure
// marks the job FAILED so it never hangs in PENDING.
//
// Two contexts are used deliberately:
//   - callCtx: bounds the HTTP submit so a stuck TC cannot pin this goroutine
//     forever; canceled on return.
//   - markFailed (a seam, see markForwardBuildJobFailed) uses a fresh
//     context.Background()-derived context, never callCtx. Reusing callCtx is
//     wrong: by the time the 60s submit timeout fires, every DB write against
//     callCtx would be dropped, so the job would stay PENDING -- exactly what
//     markForwardBuildJobFailed exists to prevent.
//
// updateTemplateImageJobFn is the lowest seam: the actual DB write. Tests stub
// THIS (not markForwardBuildJobFailed) so the real context-creation logic in
// markForwardBuildJobFailed runs and the ctx it produces can be observed.
// Stubbing markForwardBuildJobFailed itself would bypass exactly the code under
// test, which is the mistake that made the original tests assert the caller's
// deadline-bound ctx instead of the fresh one.
var updateTemplateImageJobFn = templatecenter.UpdateTemplateImageJob

// markForwardBuildJobFailed marks the job FAILED. It derives its context from
// context.Background(), NOT from the caller's callCtx: by the time the submit's
// 60s deadline fires, a callCtx-derived write would be dropped and the job
// would stay PENDING -- exactly what this function exists to prevent. Asserted
// by TestForwardBuildJobFailed*.
func markForwardBuildJobFailed(ctx context.Context, jobID, msg string) {
	// A fresh, deadline-free context derived from Background: must NOT inherit
	// the submit timeout. The incoming ctx is used only for logging context.
	failCtx, failCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer failCancel()
	if err := updateTemplateImageJobFn(failCtx, jobID, map[string]any{
		"status":        templatecenter.JobStatusFailed,
		"error_message": msg,
	}); err != nil {
		log.G(failCtx).Errorf("forward to templatecenter: mark job failed fail: job_id=%s err=%v", jobID, err)
	}
}

func forwardBuildJobToTemplateCenter(jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdPayload *templatecenter.EnvdInjectionPayload) {
	callCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.GetConfig()
	endpoint := ""
	if cfg != nil {
		endpoint = cfg.TemplateCenterAddr()
	}

	if endpoint == "" {
		// Log the raw env value (with %q to expose invisible chars/trailing
		// whitespace) so an operator can tell "not set in this process" apart
		// from "set but malformed". The error returned to the client stays
		// generic (no env echo in the API response).
		raw := os.Getenv(config.EnvTemplateCenterAddr)
		log.G(callCtx).Errorf(
			"forward to templatecenter: %s is empty in THIS process (raw=%q, len=%d), job_id=%s; "+
				"the variable must be present in the CubeMaster process environment (e.g. systemd unit / container env / supervisor), "+
				"not just in an interactive shell",
			config.EnvTemplateCenterAddr, raw, len(raw), jobID)
		markForwardBuildJobFailed(callCtx, jobID, "templatecenter_enabled=true but "+config.EnvTemplateCenterAddr+" is not configured")
		return
	}

	var envdSHA string
	var envdData []byte
	if envdPayload != nil {
		envdSHA = envdPayload.SHA256
		envdData = envdPayload.Data
	}

	if err := tcclient.NewClient(endpoint).SubmitBuildJob(callCtx, jobID, req, downloadBaseURL, envdSHA, envdData); err != nil {
		log.G(callCtx).Errorf("forward to templatecenter fail: job_id=%s endpoint=%s err=%v", jobID, endpoint, err)
		markForwardBuildJobFailed(callCtx, jobID, "forward build job to templatecenter: "+err.Error())
		return
	}
	log.G(callCtx).Infof("build job forwarded to templatecenter: job_id=%s endpoint=%s", jobID, endpoint)
}

// forwardRedoBuildJobToTemplateCenter forwards a redo job to CubeTemplateCenter.
// The redo job's RequestJSON (the original create request) is loaded from the
// database so TC receives the exact same payload as a fresh create.
func forwardRedoBuildJobToTemplateCenter(jobID string, downloadBaseURL string) {
	callCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.GetConfig()
	endpoint := ""
	if cfg != nil {
		endpoint = cfg.TemplateCenterAddr()
	}

	if endpoint == "" {
		raw := os.Getenv(config.EnvTemplateCenterAddr)
		log.G(callCtx).Errorf(
			"forward redo to templatecenter: %s is empty in THIS process (raw=%q, len=%d), job_id=%s; "+
				"the variable must be present in the CubeMaster process environment, not just an interactive shell",
			config.EnvTemplateCenterAddr, raw, len(raw), jobID)
		markForwardBuildJobFailed(callCtx, jobID, "templatecenter_enabled=true but "+config.EnvTemplateCenterAddr+" is not configured")
		return
	}

	// Load the persisted job snapshot to recover the original create request.
	job, err := templatecenter.GetTemplateImageJobRecordByID(callCtx, jobID)
	if err != nil {
		log.G(callCtx).Errorf("forward redo to templatecenter: load job %s fail: %v", jobID, err)
		markForwardBuildJobFailed(callCtx, jobID, "load redo job for forwarding: "+err.Error())
		return
	}
	if job.RequestJSON == "" {
		log.G(callCtx).Errorf("forward redo to templatecenter: job %s has empty request_json", jobID)
		markForwardBuildJobFailed(callCtx, jobID, "redo job has empty request_json")
		return
	}

	var createReq types.CreateTemplateFromImageReq
	if err := json.Unmarshal([]byte(job.RequestJSON), &createReq); err != nil {
		log.G(callCtx).Errorf("forward redo to templatecenter: decode job %s request_json fail: %v", jobID, err)
		markForwardBuildJobFailed(callCtx, jobID, "decode redo job request_json: "+err.Error())
		return
	}

	if err := tcclient.NewClient(endpoint).SubmitBuildJob(callCtx, jobID, &createReq, downloadBaseURL, "", nil); err != nil {
		log.G(callCtx).Errorf("forward redo to templatecenter fail: job_id=%s endpoint=%s err=%v", jobID, endpoint, err)
		markForwardBuildJobFailed(callCtx, jobID, "forward redo build job to templatecenter: "+err.Error())
		return
	}
	log.G(callCtx).Infof("redo build job forwarded to templatecenter: job_id=%s endpoint=%s", jobID, endpoint)
}
