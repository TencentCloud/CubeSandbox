// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestExternalHTTPScoreSelectUsesSidecarScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var req externalHTTPScoreRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Mode != "prefer-node-b" {
			t.Fatalf("mode = %s, want prefer-node-b", req.Mode)
		}
		if req.TemplateID != "tpl-test" {
			t.Fatalf("template_id = %s, want tpl-test", req.TemplateID)
		}
		if len(req.Nodes) != 2 {
			t.Fatalf("nodes = %d, want 2", len(req.Nodes))
		}
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
				"node-b": 90,
			},
		})
	}))
	defer server.Close()

	initExternalHTTPScoreTestConfig(t, server.URL)

	scorer := NewExternalHTTPScore()
	got, err := scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
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

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
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

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
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

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
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

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
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

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want empty scores error")
	}
}

func TestExternalHTTPScoreRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()

	initExternalHTTPScoreTestConfig(t, server.URL)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
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

	initExternalHTTPScoreTestConfigWithPluginConfig(t, `
        weight: 1
        endpoint: "`+server.URL+`"
        timeout: 10ms
        mode: prefer-node-b
`)

	_, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want timeout error")
	}
}

func TestExternalHTTPScoreSkipsWhenEndpointEmpty(t *testing.T) {
	initExternalHTTPScoreTestConfig(t, "")

	got, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreSkipsWhenDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("disabled external HTTP scorer should not call endpoint")
	}))
	defer server.Close()

	initExternalHTTPScoreTestConfigWithPluginConfig(t, `
        weight: 1
        endpoint: "`+server.URL+`"
        timeout: 1s
        mode: prefer-node-b
        disable: true
`)

	got, err := NewExternalHTTPScore().Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreRegisteredByConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]float64{
				"node-a": 10,
				"node-b": 90,
			},
		})
	}))
	defer server.Close()

	initExternalHTTPScoreTestConfig(t, server.URL)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 {
		t.Fatalf("len(selectors) = %d, want 1", len(selectors))
	}
	if selectors[0].ID() != constants.SelectorScoreID+"/"+externalHTTPScoreName {
		t.Fatalf("selector ID = %s, want external_http_score", selectors[0].ID())
	}
}

func TestExternalHTTPScoreNotEnabledByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("external HTTP scorer should not be called unless it is enabled")
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  score:
    resource_weights:
      external_http_score: 1
    plugin_conf:
      external_http_score:
        weight: 1
        endpoint: "` + server.URL + `"
        timeout: 1s
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}

	selectors := NewSelector(context.Background())
	if len(selectors) != 0 {
		t.Fatalf("len(selectors) = %d, want 0", len(selectors))
	}
}

func initExternalHTTPScoreTestConfig(t *testing.T, endpoint string) {
	t.Helper()

	initExternalHTTPScoreTestConfigWithPluginConfig(t, `
        weight: 1
        endpoint: "`+endpoint+`"
        timeout: 1s
        mode: prefer-node-b
`)
}

func initExternalHTTPScoreTestConfigWithPluginConfig(t *testing.T, pluginConfig string) {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      external_http_score: 1
    plugin_conf:
      external_http_score:` + pluginConfig
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}
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
