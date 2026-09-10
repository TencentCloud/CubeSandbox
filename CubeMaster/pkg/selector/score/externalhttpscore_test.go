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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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
			Scores: map[string]*float64{
				"node-a": float64Ptr(10),
				"node-b": float64Ptr(90),
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
			Scores: map[string]*float64{
				"node-a": float64Ptr(101),
				"node-b": float64Ptr(90),
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want invalid score error")
	}
}

func TestExternalHTTPScoreFilterResponseContract(t *testing.T) {
	knownNodes := map[string]struct{}{
		"node-a": {},
		"node-b": {},
	}
	ctx := context.Background()

	t.Run("exact candidate set succeeds", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": float64Ptr(10),
			"node-b": float64Ptr(90),
		}, knownNodes)
		if err != nil {
			t.Fatalf("filterExternalHTTPScoreResponse() error = %v", err)
		}
		if len(got) != 2 || got["node-a"] != 10 || got["node-b"] != 90 {
			t.Fatalf("got = %#v, want exact known scores", got)
		}
	})

	t.Run("valid extra key ignored from returned map", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a":  float64Ptr(10),
			"node-b":  float64Ptr(90),
			"stale-x": float64Ptr(50),
		}, knownNodes)
		if err != nil {
			t.Fatalf("filterExternalHTTPScoreResponse() error = %v", err)
		}
		if _, ok := got["stale-x"]; ok {
			t.Fatalf("extra key leaked into returned scores: %#v", got)
		}
		if len(got) != 2 {
			t.Fatalf("got = %#v, want only known candidates", got)
		}
	})

	t.Run("extra key with invalid score ignored", func(t *testing.T) {
		nan := math.NaN()
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a":  float64Ptr(10),
			"node-b":  float64Ptr(90),
			"stale-x": &nan,
		}, knownNodes)
		if err != nil {
			t.Fatalf("filterExternalHTTPScoreResponse() error = %v, want nil when only extras are invalid", err)
		}
		if _, ok := got["stale-x"]; ok {
			t.Fatalf("invalid extra key leaked: %#v", got)
		}
	})

	t.Run("extra key with null ignored", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a":  float64Ptr(10),
			"node-b":  float64Ptr(90),
			"stale-x": nil,
		}, knownNodes)
		if err != nil {
			t.Fatalf("error = %v, want nil when only extras are null", err)
		}
		if _, ok := got["stale-x"]; ok {
			t.Fatalf("null extra key leaked: %#v", got)
		}
	})

	t.Run("known candidate null fails", func(t *testing.T) {
		_, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": nil,
			"node-b": float64Ptr(90),
		}, knownNodes)
		if err == nil {
			t.Fatal("error = nil, want invalid known null")
		}
	})

	t.Run("known candidate numeric zero succeeds", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": float64Ptr(0),
			"node-b": float64Ptr(100),
		}, knownNodes)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got["node-a"] != 0 || got["node-b"] != 100 {
			t.Fatalf("got = %#v", got)
		}
	})

	t.Run("missing known candidate fails", func(t *testing.T) {
		_, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": float64Ptr(10),
		}, knownNodes)
		if err == nil {
			t.Fatal("error = nil, want missing candidate")
		}
	})

	t.Run("invalid known candidate score fails", func(t *testing.T) {
		_, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": float64Ptr(101),
			"node-b": float64Ptr(90),
		}, knownNodes)
		if err == nil {
			t.Fatal("error = nil, want invalid known score")
		}
	})

	t.Run("empty known set returns empty map like Select", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"stale-x": float64Ptr(1),
		}, map[string]struct{}{})
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Fatalf("got = %#v, want empty", got)
		}
	})

	t.Run("boundary scores accepted", func(t *testing.T) {
		got, err := filterExternalHTTPScoreResponse(ctx, map[string]*float64{
			"node-a": float64Ptr(0),
			"node-b": float64Ptr(100),
		}, knownNodes)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got["node-a"] != 0 || got["node-b"] != 100 {
			t.Fatalf("got = %#v", got)
		}
	})
}

func TestExternalHTTPScoreSelectIgnoresExtraResponseKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{
				"node-a":  float64Ptr(10),
				"node-b":  float64Ptr(90),
				"stale-x": float64Ptr(55),
			},
		})
	}))
	defer server.Close()

	got, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}
	for _, n := range got {
		if n.ID() == "stale-x" {
			t.Fatal("extra key present in Select result")
		}
	}
}

func TestExternalHTTPScoreSelectIgnoresInvalidExtraKeys(t *testing.T) {
	// JSON cannot encode Inf/NaN; use an out-of-range finite value on an unknown
	// key so the body still unmarshals and only the extra key is invalid.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"scores":{"node-a":10,"node-b":90,"stale-x":999}}`)
	}))
	defer server.Close()

	got, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want success when only extras are invalid", err)
	}
	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}
	for _, n := range got {
		if n.ID() == "stale-x" {
			t.Fatal("invalid extra key present in Select result")
		}
	}
}

func TestExternalHTTPScoreSelectRejectsKnownNullScore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"scores":{"node-a":null,"node-b":90}}`)
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want known null score error")
	}
}

func TestExternalHTTPScoreSelectIgnoresExtraNullKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"scores":{"node-a":10,"node-b":90,"stale-x":null}}`)
	}))
	defer server.Close()

	got, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v, want success when only extras are null", err)
	}
	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}
	for _, n := range got {
		if n.ID() == "stale-x" {
			t.Fatal("extra null key present in Select result")
		}
	}
}

func TestExternalHTTPScoreSelectAcceptsKnownNumericZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"scores":{"node-a":0,"node-b":100}}`)
	}))
	defer server.Close()

	got, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}
	byID := map[string]float64{}
	for _, n := range got {
		byID[n.ID()] = n.Score
	}
	if byID["node-a"] != 0 || byID["node-b"] != 100 {
		t.Fatalf("scores = %#v, want node-a=0 node-b=100", byID)
	}
}

func TestExternalHTTPScoreRejectsNonNumericScoreValue(t *testing.T) {
	cases := []string{
		`{"scores":{"node-a":"10","node-b":90}}`,
		`{"scores":{"node-a":{"v":10},"node-b":90}}`,
		`{"scores":{"node-a":[10],"node-b":90}}`,
	}
	for _, body := range cases {
		body := body
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
			if err == nil {
				t.Fatal("Select() error = nil, want malformed decode error")
			}
			if !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("error = %q, want malformed", err)
			}
		})
	}
}

func TestExternalHTTPScoreRejectsMissingNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{
				"node-a": float64Ptr(10),
			},
		})
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want missing node error")
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

func TestExternalHTTPScoreRejectsEmptyScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{},
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
			Scores: map[string]*float64{
				"node-a": float64Ptr(10),
				"node-b": float64Ptr(90),
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
			Scores: map[string]*float64{
				"node-a": float64Ptr(10),
				"node-b": float64Ptr(90),
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

func TestExternalHTTPScoreRegistryMapsToNewExternalHTTPScore(t *testing.T) {
	ctor, ok := scores[externalHTTPScoreName]
	if !ok || ctor == nil {
		t.Fatal("external_http_score missing from package registry")
	}
	if reflect.ValueOf(ctor).Pointer() != reflect.ValueOf(NewExternalHTTPScore).Pointer() {
		t.Fatal("registry factory is not NewExternalHTTPScore")
	}
}

func TestExternalHTTPScoreRegistryFactoryPanicsWhenExternalPluginMissing(t *testing.T) {
	// Ideal unit-test state is GetConfig()==nil. This package's asyncscore_test
	// init() always loads conf.yaml, so the global is usually non-nil here.
	// Require only that plugin_conf.external_http_score stays absent so the
	// production constructor panic can run without mutating globals.
	if externalHTTPScoreConfigFrom(config.GetConfig()) != nil {
		t.Fatal("global config already defines external_http_score; refusing to mutate it")
	}
	ctor, ok := scores[externalHTTPScoreName]
	if !ok || ctor == nil {
		t.Fatal("external_http_score missing from package registry")
	}
	fn := reflect.ValueOf(ctor)
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		t.Fatalf("registry factory kind = %v, want func", fn.Kind())
	}
	ft := fn.Type()
	if ft.NumIn() != 0 || ft.NumOut() != 1 {
		t.Fatalf("registry factory signature = %s, want func() T matching NewSelector Call(nil)", ft)
	}
	selType := reflect.TypeOf((*Selector)(nil)).Elem()
	if !ft.Out(0).Implements(selType) {
		t.Fatalf("registry factory returns %s, which does not implement Selector", ft.Out(0))
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic from invoking registered factory when external_http_score plugin is missing")
		}
		if !strings.Contains(fmt.Sprint(r), "ExternalHTTPScore is nil") {
			t.Fatalf("panic = %v, want ExternalHTTPScore is nil", r)
		}
	}()
	// Same mechanics as NewSelector: reflect.ValueOf(scores[name]).Call(nil).
	_ = fn.Call(nil)
}

func TestNewExternalHTTPScoreFromConfigPanicsWhenPluginMissing(t *testing.T) {
	tests := []struct {
		name   string
		global *config.Config
	}{
		{name: "nil global", global: nil},
		{name: "nil scheduler", global: &config.Config{}},
		{name: "nil score", global: &config.Config{Scheduler: &config.WrapperSchedulerConf{}}},
		{
			name: "nil external plugin",
			global: &config.Config{Scheduler: &config.WrapperSchedulerConf{
				SchedulerConf: config.SchedulerConf{
					Score: &config.SchedulerScoreConf{},
				},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("want panic")
				}
				if !strings.Contains(fmt.Sprint(r), "ExternalHTTPScore is nil") {
					t.Fatalf("panic = %v, want ExternalHTTPScore is nil", r)
				}
			}()
			_ = newExternalHTTPScoreFromConfig(tt.global)
		})
	}
}

func TestNewExternalHTTPScoreFromConfigUsesPluginWeightAndID(t *testing.T) {
	plugin := &config.ExternalHTTPScore{Weight: 3.5, Endpoint: "http://example.invalid"}
	global := &config.Config{Scheduler: &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			Score: &config.SchedulerScoreConf{
				ScorePluginConf: config.ScorePluginConf{ExternalHTTPScore: plugin},
			},
		},
	}}
	scorer := newExternalHTTPScoreFromConfig(global)
	if scorer.Weight() != 3.5 {
		t.Fatalf("Weight() = %v, want 3.5", scorer.Weight())
	}
	if scorer.ID() != constants.SelectorScoreID+"/"+externalHTTPScoreName {
		t.Fatalf("ID() = %s", scorer.ID())
	}
	if scorer.cfg != nil {
		t.Fatal("production constructor must not inject cfg snapshot field")
	}
	if externalHTTPScoreConfigFrom(global) != plugin {
		t.Fatal("constructor must use externalHTTPScoreConfigFrom")
	}
}

func TestExternalHTTPScoreSharedHTTPClientTransport(t *testing.T) {
	if externalHTTPScoreHTTPClient == nil {
		t.Fatal("shared client is nil")
	}
	if externalHTTPScoreHTTPClient.Timeout != 0 {
		t.Fatalf("shared client Timeout = %v, want 0 (per-request context only)", externalHTTPScoreHTTPClient.Timeout)
	}
	tr, ok := externalHTTPScoreHTTPClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Transport = %T, want dedicated *http.Transport", externalHTTPScoreHTTPClient.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("shared transport must not be http.DefaultTransport")
	}
	if def, ok := http.DefaultTransport.(*http.Transport); ok && tr == def {
		t.Fatal("shared transport must be a clone, not DefaultTransport")
	}
	if tr.MaxIdleConns != externalHTTPScoreMaxIdleConns {
		t.Fatalf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, externalHTTPScoreMaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != externalHTTPScoreMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, externalHTTPScoreMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != externalHTTPScoreIdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, externalHTTPScoreIdleConnTimeout)
	}
}

func TestSanitizeExternalHTTPScoreFailureOmitsEndpointSecrets(t *testing.T) {
	const sentinel = "DO_NOT_LOG_THIS"
	const host = "sidecar.example"
	secretURL := "https://user:pass@" + host + "/score?token=" + sentinel

	tests := []struct {
		name      string
		err       error
		wantExact string
	}{
		{
			name: "deadline exceeded",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: context.DeadlineExceeded,
			},
			wantExact: "http_Post_timeout",
		},
		{
			name: "nested dial text with sentinel",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: errors.New("dial " + secretURL + " token=" + sentinel),
			},
			wantExact: "http_Post_failed",
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
			wantExact: "http_Get_failed",
		},
		{
			name: "context canceled",
			err: &url.Error{
				Op:  "Post",
				URL: secretURL,
				Err: context.Canceled,
			},
			wantExact: "http_Post_canceled",
		},
		{
			name:      "missing candidate with secret suffix",
			err:       errors.New("external_http_score missing candidate score token=" + sentinel),
			wantExact: "external_http_score missing_candidate",
		},
		{
			name:      "invalid score with secret suffix",
			err:       errors.New("external_http_score invalid score for known candidate token=" + sentinel),
			wantExact: "external_http_score invalid_candidate_score",
		},
		{
			name:      "unexpected status with secret suffix",
			err:       errors.New("external_http_score unexpected status token=" + sentinel),
			wantExact: "external_http_score unexpected_status",
		},
		{
			name:      "response exceeds with secret and byte count",
			err:       errors.New("external_http_score response exceeds 1048576 bytes token=" + sentinel),
			wantExact: "external_http_score response_too_large",
		},
		{
			name:      "malformed with secret suffix",
			err:       errors.New("external_http_score malformed response: " + sentinel),
			wantExact: "external_http_score malformed_response",
		},
		{
			name:      "empty scores with secret suffix",
			err:       errors.New("external_http_score response scores is empty token=" + sentinel),
			wantExact: "external_http_score empty_scores",
		},
		{
			name:      "nil selector context with secret",
			err:       errors.New("external_http_score: selector context is nil token=" + sentinel),
			wantExact: "external_http_score nil_selector_context",
		},
		{
			name:      "other external_http_score colon prefix",
			err:       errors.New("external_http_score: boom token=" + sentinel),
			wantExact: "external_http_score request_failed",
		},
		{
			name:      "wrapped unexpected status",
			err:       fmt.Errorf("wrap-%s: %w", sentinel, errors.New("external_http_score unexpected status: 502 token="+sentinel)),
			wantExact: "external_http_score unexpected_status",
		},
		{
			name:      "wrapped malformed with nested cause",
			err:       fmt.Errorf("external_http_score malformed response: %w", errors.New("json token="+sentinel)),
			wantExact: "external_http_score malformed_response",
		},
		{
			name:      "unknown error with secret must not leak",
			err:       errors.New("totally unknown failure token=" + sentinel),
			wantExact: "http_request_failed",
		},
	}

	forbidden := []string{sentinel, secretURL, host, "user:pass", "token=", "502", "1048576"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeExternalHTTPScoreFailure(tt.err)
			if got != tt.wantExact {
				t.Fatalf("sanitized = %q, want exact %q", got, tt.wantExact)
			}
			for _, bad := range forbidden {
				if strings.Contains(got, bad) {
					t.Fatalf("sanitized = %q, must not contain %q", got, bad)
				}
			}
			// Guard against a future return-msg regression: output must be a
			// short fixed token without the original error text.
			if strings.Contains(got, "token=") || strings.Contains(got, sentinel) {
				t.Fatalf("sanitized leaked marker text: %q", got)
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

func float64Ptr(v float64) *float64 {
	return &v
}
