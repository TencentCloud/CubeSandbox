// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

const externalHTTPScoreName = "external_http_score"

// defaultExternalHTTPScoreTimeout is used when plugin_conf.timeout is zero or omitted.
const defaultExternalHTTPScoreTimeout = 200 * time.Millisecond

// maxExternalHTTPScoreResponseBytes bounds the HTTP response body on the create
// path. Bodies larger than this are rejected entirely (no partial JSON accept).
const maxExternalHTTPScoreResponseBytes = 1 << 20 // 1 MiB

type externalHTTPScore struct {
	weight float64
	// cfg is an optional immutable plugin config used by tests. Production
	// constructors leave it nil and read live config on each Select call.
	cfg *config.ExternalHTTPScore
}

type externalHTTPScoreRequest struct {
	Mode         string                  `json:"mode,omitempty"`
	InstanceType string                  `json:"instance_type,omitempty"`
	TemplateID   string                  `json:"template_id,omitempty"`
	Nodes        []externalHTTPScoreNode `json:"nodes"`
}

type externalHTTPScoreNode struct {
	NodeID              string `json:"node_id"`
	NodeIP              string `json:"node_ip,omitempty"`
	InstanceType        string `json:"instance_type,omitempty"`
	MvmNum              int64  `json:"mvm_num"`
	RealTimeCreateNum   int64  `json:"real_time_create_num"`
	LocalCreateNum      int64  `json:"local_create_num"`
	CreateConcurrentNum int64  `json:"create_concurrent_num"`
	QuotaCPU            int64  `json:"quota_cpu"`
	QuotaMem            int64  `json:"quota_mem"`
	// QuotaCPUUsage / QuotaMemUsage are raw reported counters from the node
	// snapshot. They are intentionally not passed through
	// SchedulerConf.EffectiveAllocated, so they can differ from built-in
	// scorers when ignore_redis_allocation is true.
	QuotaCPUUsage int64   `json:"quota_cpu_usage"`
	QuotaMemUsage int64   `json:"quota_mem_usage"`
	CPUUtil       float64 `json:"cpu_util"`
	MemUsage      int64   `json:"mem_usage"`
}

type externalHTTPScoreResponse struct {
	Scores map[string]float64 `json:"scores"`
}

func NewExternalHTTPScore() *externalHTTPScore {
	if config.GetConfig().Scheduler.Score.ScorePluginConf.ExternalHTTPScore == nil {
		panic("config.Scheduler.Score.ScorePluginConf.ExternalHTTPScore is nil")
	}
	return &externalHTTPScore{
		weight: config.GetConfig().Scheduler.Score.ScorePluginConf.ExternalHTTPScore.Weight,
	}
}

// newExternalHTTPScoreWithConfig constructs a scorer with an immutable config
// snapshot. Package-private test seam; production continues to use NewExternalHTTPScore.
func newExternalHTTPScoreWithConfig(cfg *config.ExternalHTTPScore) *externalHTTPScore {
	if cfg == nil {
		panic("external_http_score config is nil")
	}
	return &externalHTTPScore{
		weight: cfg.Weight,
		cfg:    cfg,
	}
}

func (l *externalHTTPScore) ID() string {
	return constants.SelectorScoreID + "/" + externalHTTPScoreName
}

func (l *externalHTTPScore) String() string {
	return l.ID()
}

func (l *externalHTTPScore) Weight() float64 {
	return l.weight
}

func (l *externalHTTPScore) pluginConfig() *config.ExternalHTTPScore {
	if l.cfg != nil {
		return l.cfg
	}
	return externalHTTPScoreConfigFrom(config.GetConfig())
}

// externalHTTPScoreConfigFrom reads the live plugin block from a global config
// snapshot. It is nil-safe at every level so callers never dereference nil.
func externalHTTPScoreConfigFrom(global *config.Config) *config.ExternalHTTPScore {
	if global == nil || global.Scheduler == nil || global.Scheduler.Score == nil {
		return nil
	}
	return global.Scheduler.Score.ScorePluginConf.ExternalHTTPScore
}

func (l *externalHTTPScore) Disable() bool {
	cfg := l.pluginConfig()
	return cfg == nil || cfg.Disable
}

func (l *externalHTTPScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "externalHTTPScore panic:%s", r)
		}
	}()

	ctx := context.Background()
	if selCtx != nil && selCtx.Ctx != nil {
		ctx = selCtx.Ctx
	}

	if selCtx == nil {
		err = fmt.Errorf("external_http_score: selector context is nil")
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}

	cfg := l.pluginConfig()
	if cfg == nil || l.Disable() || cfg.Endpoint == "" {
		return nil, nil
	}

	inList := selCtx.Nodes()
	if inList.Len() == 0 {
		return nil, nil
	}

	reqBody, knownNodes := buildExternalHTTPScoreRequest(selCtx, cfg.Mode, inList)
	respScores, err := requestExternalHTTPScores(ctx, cfg.Endpoint, cfg.Timeout, reqBody)
	if err != nil {
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}
	if err := validateExternalHTTPScoreResponse(respScores, knownNodes); err != nil {
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}

	nodes = make(node.NodeScoreList, 0, inList.Len())
	for _, n := range inList {
		score, ok := respScores[n.ID()]
		if !ok {
			err := fmt.Errorf("external_http_score missing node score: %s", n.ID())
			logExternalHTTPScoreFailure(ctx, err)
			return nil, err
		}
		nodes.Append(&node.NodeScore{
			InsID:    n.ID(),
			Score:    score,
			MvmNum:   n.MvmNum,
			OrigNode: n,
		})
	}
	return nodes, nil
}

func logExternalHTTPScoreFailure(ctx context.Context, err error) {
	log.G(ctx).Warnf("external_http_score fail-open: %s", sanitizeExternalHTTPScoreFailure(err))
}

// sanitizeExternalHTTPScoreFailure returns a log-safe failure summary that never
// includes raw endpoints, URL userinfo, query parameters, or arbitrary nested
// error text that may embed those values. Categories are allow-listed.
func sanitizeExternalHTTPScoreFailure(err error) string {
	if err == nil {
		return "unknown_error"
	}
	if cat, ok := classifyExternalHTTPScoreURLError(err, ""); ok {
		return cat
	}
	// Non-URL errors that already look like our own validation messages are safe.
	msg := err.Error()
	if strings.HasPrefix(msg, "external_http_score ") || strings.HasPrefix(msg, "external_http_score:") {
		return msg
	}
	return "http_request_failed"
}

func classifyExternalHTTPScoreURLError(err error, op string) (string, bool) {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		if op == "" {
			return "", false
		}
		// Nested cause under a url.Error: never format cause text; classify only.
		return httpFailureCategory(op, err), true
	}
	nextOp := urlErr.Op
	if nextOp == "" {
		nextOp = op
	}
	if nextOp == "" {
		nextOp = "request"
	}
	if urlErr.Err == nil {
		return fmt.Sprintf("http_%s_failed", nextOp), true
	}
	// Recurse into nested *url.Error without ever interpolating Err text.
	if cat, ok := classifyExternalHTTPScoreURLError(urlErr.Err, nextOp); ok {
		return cat, true
	}
	return httpFailureCategory(nextOp, urlErr.Err), true
}

func httpFailureCategory(op string, cause error) string {
	switch {
	case cause == nil:
		return fmt.Sprintf("http_%s_failed", op)
	case errors.Is(cause, context.DeadlineExceeded):
		return fmt.Sprintf("http_%s_timeout", op)
	case errors.Is(cause, context.Canceled):
		return fmt.Sprintf("http_%s_canceled", op)
	case isConnectionRefused(cause):
		return fmt.Sprintf("http_%s_connection_refused", op)
	case isDNSOrTransport(cause):
		return fmt.Sprintf("http_%s_transport_failed", op)
	default:
		return fmt.Sprintf("http_%s_failed", op)
	}
}

func isConnectionRefused(err error) bool {
	// Message inspected only for category selection; never logged.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused")
}

func isDNSOrTransport(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func validateExternalHTTPScoreResponse(scores map[string]float64, knownNodes map[string]struct{}) error {
	if len(scores) != len(knownNodes) {
		return fmt.Errorf("external_http_score score count mismatch: got %d want %d", len(scores), len(knownNodes))
	}
	for nodeID, score := range scores {
		if _, ok := knownNodes[nodeID]; !ok {
			return fmt.Errorf("external_http_score unknown node: %s", nodeID)
		}
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 100 {
			return fmt.Errorf("external_http_score invalid score for node %s: %f", nodeID, score)
		}
	}
	return nil
}

func buildExternalHTTPScoreRequest(selCtx *selctx.SelectorCtx, mode string, inList node.NodeList) (externalHTTPScoreRequest, map[string]struct{}) {
	req := externalHTTPScoreRequest{
		Mode:         mode,
		InstanceType: selCtx.InstanceType,
		Nodes:        make([]externalHTTPScoreNode, 0, inList.Len()),
	}
	if selCtx.ReqRes != nil {
		req.TemplateID = selCtx.ReqRes.TemplateID
	}

	knownNodes := make(map[string]struct{}, inList.Len())
	for _, n := range inList {
		nodeID := n.ID()
		knownNodes[nodeID] = struct{}{}
		req.Nodes = append(req.Nodes, externalHTTPScoreNode{
			NodeID:              nodeID,
			NodeIP:              n.IP,
			InstanceType:        n.InstanceType,
			MvmNum:              n.MvmNum,
			RealTimeCreateNum:   n.RealTimeCreateNum,
			LocalCreateNum:      n.LocalCreateNum,
			CreateConcurrentNum: n.CreateConcurrentNum,
			QuotaCPU:            n.QuotaCpu,
			QuotaMem:            n.QuotaMem,
			QuotaCPUUsage:       n.QuotaCpuUsage,
			QuotaMemUsage:       n.QuotaMemUsage,
			CPUUtil:             n.CpuUtil,
			MemUsage:            n.MemUsage,
		})
	}
	return req, knownNodes
}

func requestExternalHTTPScores(ctx context.Context, endpoint string, timeout time.Duration, reqBody externalHTTPScoreRequest) (map[string]float64, error) {
	if timeout <= 0 {
		timeout = defaultExternalHTTPScoreTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Do not follow redirects for the fixed sidecar endpoint.
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("external_http_score unexpected status: %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(maxExternalHTTPScoreResponseBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxExternalHTTPScoreResponseBytes {
		return nil, fmt.Errorf("external_http_score response exceeds %d bytes", maxExternalHTTPScoreResponseBytes)
	}

	var out externalHTTPScoreResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("external_http_score malformed response: %w", err)
	}
	if len(out.Scores) == 0 {
		return nil, fmt.Errorf("external_http_score response scores is empty")
	}
	return out.Scores, nil
}
