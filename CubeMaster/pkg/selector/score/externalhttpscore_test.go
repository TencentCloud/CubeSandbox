// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestExternalHTTPScoreSelectUsesSidecarScores(t *testing.T) {
	type captured struct {
		method string
		req    externalHTTPScoreRequest
		err    string
	}
	ch := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req externalHTTPScoreRequest
		obs := captured{method: r.Method}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			obs.err = err.Error()
			ch <- obs
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		obs.req = req
		ch <- obs
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
				"node-b": 90,
			},
		})
	}))
	defer server.Close()

	scorer := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: server.URL,
		Timeout:  time.Second,
		Mode:     "prefer-node-b",
	})
	got, err := scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	obs := <-ch
	if obs.err != "" {
		t.Fatalf("handler decode error: %s", obs.err)
	}
	if obs.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", obs.method)
	}
	if obs.req.Mode != "prefer-node-b" {
		t.Fatalf("mode = %s, want prefer-node-b", obs.req.Mode)
	}
	if obs.req.TemplateID != "tpl-test" {
		t.Fatalf("template_id = %s, want tpl-test", obs.req.TemplateID)
	}
	if len(obs.req.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(obs.req.Nodes))
	}

	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}
	if got[0].ID() != "node-a" || got[0].Score != 10 {
		t.Fatalf("got first score %+v, want node-a=10", got[0])
	}
	if got[1].ID() != "node-b" || got[1].Score != 90 {
		t.Fatalf("got second score %+v, want node-b=90", got[1])
	}

	sorted := got.AllSortByScore()
	if sorted[0].ID() != "node-b" {
		t.Fatalf("highest score node = %s, want node-b", sorted[0].ID())
	}
}

func TestExternalHTTPScoreRejectsInvalidScore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 101,
				"node-b": 90,
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want invalid score error")
	}
}

func TestExternalHTTPScoreValidateResponseBoundaries(t *testing.T) {
	knownNodes := map[string]struct{}{
		"node-a": {},
		"node-b": {},
	}

	tests := []struct {
		name    string
		scores  map[string]float64
		wantErr bool
	}{
		{
			name: "accepts zero and one hundred",
			scores: map[string]float64{
				"node-a": 0,
				"node-b": 100,
			},
		},
		{
			name: "rejects negative score",
			scores: map[string]float64{
				"node-a": -1,
				"node-b": 100,
			},
			wantErr: true,
		},
		{
			name: "rejects NaN score",
			scores: map[string]float64{
				"node-a": math.NaN(),
				"node-b": 100,
			},
			wantErr: true,
		},
		{
			name: "rejects positive infinity score",
			scores: map[string]float64{
				"node-a": math.Inf(1),
				"node-b": 100,
			},
			wantErr: true,
		},
		{
			name: "rejects negative infinity score",
			scores: map[string]float64{
				"node-a": math.Inf(-1),
				"node-b": 100,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExternalHTTPScoreResponse(tt.scores, knownNodes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateExternalHTTPScoreResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestExternalHTTPScoreRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want HTTP status error")
	}
}

func TestExternalHTTPScoreRejectsUnknownNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a":  10,
				"unknown": 90,
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want unknown node error")
	}
}

func TestExternalHTTPScoreRejectsMissingNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want missing node error")
	}
}

func TestExternalHTTPScoreRejectsEmptyScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want empty scores error")
	}
}

func TestExternalHTTPScoreRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want malformed JSON error")
	}
}

func TestExternalHTTPScoreTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
				"node-b": 90,
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: server.URL,
		Timeout:  10 * time.Millisecond,
		Mode:     "prefer-node-b",
	}).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want timeout error")
	}
}

func TestExternalHTTPScoreSkipsWhenEndpointEmpty(t *testing.T) {
	got, err := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: "",
		Timeout:  time.Second,
	}).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreSkipsWhenDisabled(t *testing.T) {
	var contacted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	got, err := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: server.URL,
		Timeout:  time.Second,
		Mode:     "prefer-node-b",
		Disable:  true,
	}).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil scores", got)
	}
	if contacted.Load() {
		t.Fatal("disabled external HTTP scorer contacted endpoint")
	}
}

func TestExternalHTTPScoreRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"scores":{"node-a":10,"node-b":90},"pad":"`)
		_, _ = w.Write(bytesRepeat('x', maxExternalHTTPScoreResponseBytes))
		_, _ = io.WriteString(w, `"}`)
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want oversized response error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Select() error = %q, want exceeds message", err)
	}
}

func TestExternalHTTPScoreDoesNotFollowRedirects(t *testing.T) {
	var redirectTargetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHit.Store(true)
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
				"node-b": 90,
			},
		})
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(redirector.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want redirect/non-2xx error")
	}
	if !strings.Contains(err.Error(), "unexpected status") {
		t.Fatalf("Select() error = %q, want unexpected status", err)
	}
	if redirectTargetHit.Load() {
		t.Fatal("redirect target was contacted; redirects must be disabled")
	}
}

func TestExternalHTTPScoreRegistryPresenceDoesNotEnableExecution(t *testing.T) {
	ctor, ok := scores[externalHTTPScoreName]
	if !ok || ctor == nil {
		t.Fatal("external_http_score missing from package registry")
	}

	var contacted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
	}))
	defer server.Close()

	// Registry presence alone must not cause HTTP traffic: empty endpoint and
	// disable both short-circuit without contacting a sidecar.
	empty := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: "",
		Timeout:  time.Second,
	})
	if got, err := empty.Select(externalHTTPScoreTestCtx()); err != nil || got != nil {
		t.Fatalf("empty endpoint Select() = (%v, %v), want (nil, nil)", got, err)
	}

	disabled := newExternalHTTPScoreWithConfig(&config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: server.URL,
		Timeout:  time.Second,
		Disable:  true,
	})
	if got, err := disabled.Select(externalHTTPScoreTestCtx()); err != nil || got != nil {
		t.Fatalf("disabled Select() = (%v, %v), want (nil, nil)", got, err)
	}
	if contacted.Load() {
		t.Fatal("registry presence / inactive config must not contact endpoint")
	}
}

func TestSanitizeExternalHTTPScoreFailureOmitsEndpointSecrets(t *testing.T) {
	const sentinel = "DO_NOT_LOG_THIS"
	const host = "sidecar.example"
	secretURL := "https://user:pass@" + host + "/score?token=" + sentinel

	tests := []struct {
		name       string
		err        error
		wantSubstr string
	}{
		{
			name: "deadline exceeded",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: context.DeadlineExceeded,
			},
			wantSubstr: "http_Post_timeout",
		},
		{
			name: "nested dial text with sentinel",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: errors.New("dial " + secretURL + " token=" + sentinel),
			},
			wantSubstr: "http_Post_failed",
		},
		{
			name: "nested url.Error with sentinel inner text",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: &url.Error{
					Op:  "Get",
					URL: secretURL,
					Err: errors.New("upstream token=" + sentinel + " host=" + host),
				},
			},
			wantSubstr: "http_Get_failed",
		},
		{
			name: "context canceled",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: context.Canceled,
			},
			wantSubstr: "http_Post_canceled",
		},
		{
			name:       "safe validation status",
			err:        fmt.Errorf("external_http_score unexpected status: 502"),
			wantSubstr: "external_http_score unexpected status: 502",
		},
	}

	forbidden := []string{sentinel, secretURL, host, "user:pass", "token="}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeExternalHTTPScoreFailure(tt.err)
			if !strings.Contains(got, tt.wantSubstr) {
				t.Fatalf("sanitized = %q, want substring %q", got, tt.wantSubstr)
			}
			for _, bad := range forbidden {
				if strings.Contains(got, bad) {
					t.Fatalf("sanitized = %q, must not contain %q", got, bad)
				}
			}
		})
	}
}

func TestExternalHTTPScoreSelectNilContextIsSafe(t *testing.T) {
	scorer := newExternalHTTPScoreWithConfig(testPluginConfig("http://127.0.0.1:9/unused"))
	got, err := scorer.Select(nil)
	if err == nil {
		t.Fatal("Select(nil) error = nil, want error")
	}
	if got != nil {
		t.Fatalf("Select(nil) scores = %v, want nil", got)
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Fatalf("Select(nil) error = %q, want nil-context message", err)
	}
}

func TestExternalHTTPScoreConfigFromNilSafe(t *testing.T) {
	plugin := &config.ExternalHTTPScore{Weight: 2, Endpoint: "http://example.invalid"}
	tests := []struct {
		name   string
		global *config.Config
		want   *config.ExternalHTTPScore
	}{
		{name: "nil global", global: nil, want: nil},
		{name: "nil scheduler", global: &config.Config{}, want: nil},
		{name: "nil score", global: &config.Config{Scheduler: &config.WrapperSchedulerConf{}}, want: nil},
		{
			name: "nil external plugin",
			global: &config.Config{Scheduler: &config.WrapperSchedulerConf{
				SchedulerConf: config.SchedulerConf{
					Score: &config.SchedulerScoreConf{},
				},
			}},
			want: nil,
		},
		{
			name: "populated external plugin",
			global: &config.Config{Scheduler: &config.WrapperSchedulerConf{
				SchedulerConf: config.SchedulerConf{
					Score: &config.SchedulerScoreConf{
						ScorePluginConf: config.ScorePluginConf{ExternalHTTPScore: plugin},
					},
				},
			}},
			want: plugin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := externalHTTPScoreConfigFrom(tt.global)
			if got != tt.want {
				t.Fatalf("externalHTTPScoreConfigFrom() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultExternalHTTPScoreTimeoutConstant(t *testing.T) {
	if defaultExternalHTTPScoreTimeout != 200*time.Millisecond {
		t.Fatalf("defaultExternalHTTPScoreTimeout = %v, want 200ms", defaultExternalHTTPScoreTimeout)
	}
}

func testPluginConfig(endpoint string) *config.ExternalHTTPScore {
	return &config.ExternalHTTPScore{
		Weight:   1,
		Endpoint: endpoint,
		Timeout:  time.Second,
		Mode:     "prefer-node-b",
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func externalHTTPScoreTestCtx() *selctx.SelectorCtx {
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.InstanceType = "cubebox"
	ctx.ReqRes = &selctx.RequestResource{TemplateID: "tpl-test"}
	ctx.SetNodes(node.NodeList{
		{
			InsID:               "node-a",
			IP:                  "10.0.0.1",
			InstanceType:        "cubebox",
			MvmNum:              10,
			RealTimeCreateNum:   1,
			LocalCreateNum:      1,
			CreateConcurrentNum: 30,
			QuotaCpu:            64000,
			QuotaMem:            131072,
			QuotaCpuUsage:       20000,
			QuotaMemUsage:       32768,
			CpuUtil:             30,
			MemUsage:            65536,
		},
		{
			InsID:               "node-b",
			IP:                  "10.0.0.2",
			InstanceType:        "cubebox",
			MvmNum:              2,
			RealTimeCreateNum:   0,
			LocalCreateNum:      0,
			CreateConcurrentNum: 30,
			QuotaCpu:            64000,
			QuotaMem:            131072,
			QuotaCpuUsage:       10000,
			QuotaMemUsage:       16384,
			CpuUtil:             15,
			MemUsage:            32768,
		},
	})
	return ctx
}
