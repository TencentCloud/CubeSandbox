// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

// submitBuildJobFn is a seam over tcclient.SubmitBuildJob so tests can
// simulate TC responses (transient 429/503, duplicate 409, permanent 4xx)
// without a real HTTP server.
var submitBuildJobFn = func(ctx context.Context, endpoint, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error {
	return tcclient.NewClient(endpoint).SubmitBuildJob(ctx, jobID, req, downloadBaseURL, envdSHA, envdData)
}

// forwardBuildJobRetryDelays are the backoff waits between retry attempts for
// transient TC errors (429 concurrency limit, 502/503/504 overload). Kept as
// a var so tests can shrink it.
var forwardBuildJobRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// isRetryableTCStatus reports whether a TC HTTP status represents a
// transient condition (overloaded / temporarily unavailable) worth retrying,
// as opposed to a permanent rejection (bad request, not found, etc.).
func isRetryableTCStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// submitBuildJobWithRetry submits a build job to TC, retrying transient
// errors (429/502/503/504) with backoff instead of giving up on the first
// hiccup. A 409 (TC already has this job_id in flight -- e.g. because the
// same job was forwarded twice) is treated as success, not failure: the
// build is already progressing at TC, marking it FAILED here would be wrong.
// Any other non-retryable status (4xx like bad request, or exhausted
// retries) is returned as a permanent error.
func submitBuildJobWithRetry(ctx context.Context, endpoint, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		err := submitBuildJobFn(ctx, endpoint, jobID, req, downloadBaseURL, envdSHA, envdData)
		if err == nil {
			return nil
		}
		var statusErr *tcclient.StatusError
		if errors.As(err, &statusErr) {
			if statusErr.StatusCode == http.StatusConflict {
				log.G(ctx).Infof("forward to templatecenter: job %s already submitted (409), treating as success", jobID)
				return nil
			}
			if !isRetryableTCStatus(statusErr.StatusCode) {
				return err
			}
		}
		lastErr = err
		if attempt >= len(forwardBuildJobRetryDelays) {
			return lastErr
		}
		wait := forwardBuildJobRetryDelays[attempt]
		log.G(ctx).Warnf("forward to templatecenter: transient error (attempt %d/%d), retrying in %s: job_id=%s err=%v",
			attempt+1, len(forwardBuildJobRetryDelays)+1, wait, jobID, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

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
	// 120s budget: submitBuildJobWithRetry can make up to 4 attempts against a
	// transient (429/502/503/504) TC with backoff between them; the single
	// 60s window used to leave no room for even one retry.
	callCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
		markForwardBuildJobFailed(callCtx, jobID, config.EnvTemplateCenterAddr+" is not configured; CubeMaster no longer builds templates in-process and requires CubeTemplateCenter for every build")
		return
	}

	var envdSHA string
	var envdData []byte
	if envdPayload != nil {
		envdSHA = envdPayload.SHA256
		envdData = envdPayload.Data
	}

	if err := submitBuildJobWithRetry(callCtx, endpoint, jobID, req, downloadBaseURL, envdSHA, envdData); err != nil {
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
	callCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
		markForwardBuildJobFailed(callCtx, jobID, config.EnvTemplateCenterAddr+" is not configured; CubeMaster no longer builds templates in-process and requires CubeTemplateCenter for every build")
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

	// CubeMaster never persists the uploaded envd binary: it only lives in
	// memory for the lifetime of the original multipart Create request (see
	// EnvdInjectionPayload.ReleaseData). A redo reloads createReq from the
	// DB-stored RequestJSON snapshot, which has no binary payload, so if the
	// original template opted into envd injection there is no way to
	// reproduce it here. Forwarding with envdSHA="",data=nil used to silently
	// rebuild the template WITHOUT envd baked in -- a correctness bug, not
	// just a missing feature. Fail loudly instead so the caller knows to
	// resubmit via Create (with a fresh envd upload) rather than Redo.
	if templatecenter.ShouldInjectEnvdIntoTemplate(&createReq) {
		log.G(callCtx).Errorf("forward redo to templatecenter: job %s requires envd injection but redo cannot recover the original envd binary", jobID)
		markForwardBuildJobFailed(callCtx, jobID, "redo does not support templates with envd injection: resubmit via create with a fresh envd upload")
		return
	}

	if err := submitBuildJobWithRetry(callCtx, endpoint, jobID, &createReq, downloadBaseURL, "", nil); err != nil {
		log.G(callCtx).Errorf("forward redo to templatecenter fail: job_id=%s endpoint=%s err=%v", jobID, endpoint, err)
		markForwardBuildJobFailed(callCtx, jobID, "forward redo build job to templatecenter: "+err.Error())
		return
	}
	log.G(callCtx).Infof("redo build job forwarded to templatecenter: job_id=%s endpoint=%s", jobID, endpoint)
}
