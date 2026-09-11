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
	"runtime/debug"
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

// maxExternalHTTPScoreTimeout caps the synchronous create-path HTTP budget.
// Larger values are rejected so a hung sidecar cannot add unbounded latency to
// every sandbox create.
const maxExternalHTTPScoreTimeout = 2 * time.Second

// maxExternalHTTPScoreResponseBytes bounds the HTTP response body on the create
// path. Bodies larger than this are rejected entirely (no partial JSON accept).
const maxExternalHTTPScoreResponseBytes = 1 << 20 // 1 MiB

// externalHTTPScoreErrorBodyDrainBytes is how much of a failed response body to
// read before Close so HTTP/1.1 can return the connection to the idle pool.
const externalHTTPScoreErrorBodyDrainBytes = 4 << 10

// Shared client for the synchronous create-path scorer within one CubeMaster
// process. Concurrent scheduling attempts in that process may reuse idle
// connections to the same sidecar host, so MaxIdleConnsPerHost is raised above
// the Go default of 2 while MaxIdleConns stays modest to avoid unbounded growth.
const (
	externalHTTPScoreMaxIdleConns        = 64
	externalHTTPScoreMaxIdleConnsPerHost = 8
	externalHTTPScoreIdleConnTimeout     = 90 * time.Second
)

var externalHTTPScoreHTTPClient = newExternalHTTPScoreHTTPClient()

func newExternalHTTPScoreHTTPClient() *http.Client {
	// Build a dedicated Transport instead of asserting on http.DefaultTransport.
	// Dependencies or tests may replace DefaultTransport with a wrapper
	// RoundTripper; panicking there would block CubeMaster startup even when
	// this plugin is not enabled.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          externalHTTPScoreMaxIdleConns,
		MaxIdleConnsPerHost:   externalHTTPScoreMaxIdleConnsPerHost,
		IdleConnTimeout:       externalHTTPScoreIdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		// Timeout stays on the per-call context; the shared client must not pin a
		// configuration-dependent deadline.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Do not follow redirects for the fixed sidecar endpoint.
			return http.ErrUseLastResponse
		},
	}
}

type externalHTTPScore struct {
	// cfg is an optional immutable plugin config used by tests. Production
	// constructors leave it nil and read live config on each call so weight,
	// disable, endpoint, timeout, and mode pick up conf.yaml hot-reloads.
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
	// Scores uses pointers so JSON null is distinguishable from numeric 0.
	Scores map[string]*float64 `json:"scores"`
}

func NewExternalHTTPScore() *externalHTTPScore {
	return newExternalHTTPScoreFromConfig(config.GetConfig())
}

// newExternalHTTPScoreFromConfig constructs the production scorer from a config
// snapshot. Panics when plugin_conf.external_http_score is absent (same contract
// as other score plugins that require matching plugin_conf), or when a non-empty
// endpoint / timeout fails validation.
func newExternalHTTPScoreFromConfig(global *config.Config) *externalHTTPScore {
	cfg := externalHTTPScoreConfigFrom(global)
	if cfg == nil {
		panic("config.Scheduler.Score.ScorePluginConf.ExternalHTTPScore is nil")
	}
	// Defaults are normally applied in config preHandle; re-apply here so
	// construction from a raw snapshot (tests) still sees omitted weight as
	// DefaultExternalHTTPScoreWeight.
	config.ApplyExternalHTTPScoreDefaults(cfg)
	if err := validateExternalHTTPScoreConfig(cfg); err != nil {
		panic(err.Error())
	}
	// Leave cfg nil so Weight/Disable/Select re-read live GetConfig().
	return &externalHTTPScore{}
}

// newExternalHTTPScoreWithConfig constructs a scorer with an immutable config
// snapshot. Package-private test seam; production continues to use NewExternalHTTPScore.
func newExternalHTTPScoreWithConfig(cfg *config.ExternalHTTPScore) *externalHTTPScore {
	if cfg == nil {
		panic("external_http_score config is nil")
	}
	config.ApplyExternalHTTPScoreDefaults(cfg)
	if err := validateExternalHTTPScoreConfig(cfg); err != nil {
		panic(err.Error())
	}
	return &externalHTTPScore{cfg: cfg}
}

func (l *externalHTTPScore) ID() string {
	return constants.SelectorScoreID + "/" + externalHTTPScoreName
}

func (l *externalHTTPScore) String() string {
	return l.ID()
}

func (l *externalHTTPScore) Weight() float64 {
	cfg := l.pluginConfig()
	if cfg == nil {
		return 0
	}
	if cfg.Weight == nil {
		// Omitted weight defaults via ApplyExternalHTTPScoreDefaults. Prefer the
		// filled pointer after preHandle / constructor; do not write live config.
		return config.DefaultExternalHTTPScoreWeight
	}
	return *cfg.Weight
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

// validateExternalHTTPScoreConfig checks timeout and, when endpoint is set,
// that it is an absolute http(s) URL with a host. Empty endpoint remains a
// documented no-op (Select skips the HTTP call). Side-effect free: omitted
// weight is defaulted in config.ApplyExternalHTTPScoreDefaults / preHandle,
// not here.
func validateExternalHTTPScoreConfig(cfg *config.ExternalHTTPScore) error {
	if cfg == nil {
		return fmt.Errorf("external_http_score: config is nil")
	}
	if cfg.Timeout < 0 {
		return fmt.Errorf("external_http_score: timeout must be non-negative")
	}
	if cfg.Timeout > maxExternalHTTPScoreTimeout {
		return fmt.Errorf("external_http_score: timeout must be <= %s", maxExternalHTTPScoreTimeout)
	}
	if cfg.Weight != nil && (math.IsNaN(*cfg.Weight) || math.IsInf(*cfg.Weight, 0) || *cfg.Weight < 0) {
		return fmt.Errorf("external_http_score: weight must be a finite non-negative number")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u == nil {
		return fmt.Errorf("external_http_score: invalid endpoint (require absolute http/https URL with host)")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("external_http_score: invalid endpoint (require absolute http/https URL with host)")
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("external_http_score: invalid endpoint (require absolute http/https URL with host)")
	}
	return nil
}

func (l *externalHTTPScore) Disable() bool {
	cfg := l.pluginConfig()
	if cfg == nil {
		// Scorers are constructed once and survive conf.yaml hot-reload. A
		// dropped plugin_conf.external_http_score block must not skip Select
		// silently — return false so Select can emit rate-limited observability.
		return false
	}
	// Match other scorers: Disable reflects only the disable flag. Explicit
	// weight: 0 keeps the plugin enabled but inert via Weight()/Select skip.
	return cfg.Disable
}

func (l *externalHTTPScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList, err error) {
	ctx := context.Background()
	if selCtx != nil && selCtx.Ctx != nil {
		ctx = selCtx.Ctx
	}
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "externalHTTPScore panic:%s", r)
			logExternalHTTPScoreFailure(ctx, err)
			log.G(ctx).Debugf("external_http_score panic stack:\n%s", debug.Stack())
		}
	}()

	if selCtx == nil {
		err = fmt.Errorf("external_http_score: selector context is nil")
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}

	cfg := l.pluginConfig()
	if cfg == nil {
		// Hot-reload removed the block while enable_scorers still lists us.
		err = fmt.Errorf("external_http_score plugin_conf absent")
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}
	if l.Disable() || strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, nil
	}
	if err := validateExternalHTTPScoreConfig(cfg); err != nil {
		// Hot-reload can introduce a bad endpoint/timeout after startup; fail
		// open with one sanitized log line rather than a silent no-op.
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}
	// Explicit weight: 0 (or a nil that somehow skipped defaulting) skips the
	// HTTP round trip; runScoreFilter would also multiply by Weight()==0.
	if l.Weight() <= 0 {
		return nil, nil
	}

	inList := selCtx.Nodes()
	if inList.Len() == 0 {
		return nil, nil
	}

	reqBody, knownNodes := buildExternalHTTPScoreRequest(selCtx, cfg.Mode, inList)
	respScores, err := requestExternalHTTPScores(ctx, strings.TrimSpace(cfg.Endpoint), cfg.Timeout, reqBody)
	if err != nil {
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}
	filtered, err := filterExternalHTTPScoreResponse(ctx, respScores, knownNodes)
	if err != nil {
		logExternalHTTPScoreFailure(ctx, err)
		return nil, err
	}

	nodes = make(node.NodeScoreList, 0, inList.Len())
	for _, n := range inList {
		score, ok := filtered[n.ID()]
		if !ok {
			// filterExternalHTTPScoreResponse already requires every known node;
			// this is a defensive invariant check.
			err := fmt.Errorf("external_http_score missing candidate score")
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
	cat := sanitizeExternalHTTPScoreFailure(err)
	observeExternalHTTPScoreFailure(cat)
	if shouldWarnExternalHTTPScoreFailure(cat) {
		externalHTTPScoreWarnCount.Add(1)
		log.G(ctx).Warnf("external_http_score fail-open: %s", cat)
		return
	}
	// Same category recently warned; keep create-path noise at Debug.
	log.G(ctx).Debugf("external_http_score fail-open: %s", cat)
}

// sanitizeExternalHTTPScoreFailure returns a log-safe failure summary that never
// includes raw endpoints, URL userinfo, query parameters, node IDs, or arbitrary
// nested error text that may embed those values. Categories are allow-listed;
// no branch returns err.Error() or concatenates attacker-controlled text.
func sanitizeExternalHTTPScoreFailure(err error) string {
	if err == nil {
		return "unknown_error"
	}
	if cat, ok := classifyExternalHTTPScoreURLError(err, ""); ok {
		return cat
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if cat := classifyExternalHTTPScoreMessage(e.Error()); cat != "" {
			return cat
		}
	}
	return "http_request_failed"
}

// classifyExternalHTTPScoreMessage maps known internal message prefixes to fixed
// categories. msg is inspected only for classification and never returned.
func classifyExternalHTTPScoreMessage(msg string) string {
	switch {
	case strings.Contains(msg, "externalHTTPScore panic"):
		return "external_http_score panic_recovered"
	case strings.HasPrefix(msg, "external_http_score plugin_conf absent"):
		return "external_http_score plugin_conf_absent"
	case strings.HasPrefix(msg, "external_http_score missing"):
		return "external_http_score missing_candidate"
	case strings.HasPrefix(msg, "external_http_score invalid score"):
		return "external_http_score invalid_candidate_score"
	case strings.HasPrefix(msg, "external_http_score unexpected status"):
		return "external_http_score unexpected_status"
	case strings.HasPrefix(msg, "external_http_score response exceeds"):
		return "external_http_score response_too_large"
	case strings.HasPrefix(msg, "external_http_score malformed"):
		return "external_http_score malformed_response"
	case strings.HasPrefix(msg, "external_http_score response scores is empty"):
		return "external_http_score empty_scores"
	case strings.HasPrefix(msg, "external_http_score: timeout"):
		return "external_http_score invalid_timeout"
	case strings.HasPrefix(msg, "external_http_score: weight"):
		return "external_http_score invalid_weight"
	case strings.HasPrefix(msg, "external_http_score: invalid endpoint"):
		return "external_http_score invalid_endpoint"
	case strings.HasPrefix(msg, "external_http_score:"):
		if strings.Contains(msg, "nil") {
			return "external_http_score nil_selector_context"
		}
		return "external_http_score request_failed"
	default:
		return ""
	}
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
	// nextOp is non-empty above, so the recursive call always classifies.
	return classifyExternalHTTPScoreURLError(urlErr.Err, nextOp)
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

// filterExternalHTTPScoreResponse is the single response contract: every known
// candidate must be present with a non-null finite score in [0,100]; keys
// outside knownNodes are ignored (and never returned), including extras whose
// value is null or otherwise invalid as a candidate score.
func filterExternalHTTPScoreResponse(ctx context.Context, scores map[string]*float64, knownNodes map[string]struct{}) (map[string]float64, error) {
	if len(knownNodes) == 0 {
		// Consistent with Select: an empty candidate set yields no scores.
		return map[string]float64{}, nil
	}
	out := make(map[string]float64, len(knownNodes))
	ignored := 0
	for nodeID, scorePtr := range scores {
		if _, ok := knownNodes[nodeID]; !ok {
			ignored++
			continue
		}
		if scorePtr == nil {
			return nil, fmt.Errorf("external_http_score invalid score for known candidate")
		}
		score := *scorePtr
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 100 {
			return nil, fmt.Errorf("external_http_score invalid score for known candidate")
		}
		out[nodeID] = score
	}
	for nodeID := range knownNodes {
		if _, ok := out[nodeID]; !ok {
			return nil, fmt.Errorf("external_http_score missing candidate score")
		}
	}
	if ignored > 0 {
		// Extra keys are an expected, documented condition (sidecar may echo a
		// wider node table than this request's candidate set). Log at Debug only
		// to avoid Warn spam on every scheduling attempt.
		log.G(ctx).Debugf("external_http_score ignored %d unknown score keys", ignored)
	}
	return out, nil
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

func requestExternalHTTPScores(ctx context.Context, endpoint string, timeout time.Duration, reqBody externalHTTPScoreRequest) (map[string]*float64, error) {
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

	resp, err := externalHTTPScoreHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		drainExternalHTTPScoreResponseBody(resp.Body)
		return nil, fmt.Errorf("external_http_score unexpected status: %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(maxExternalHTTPScoreResponseBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxExternalHTTPScoreResponseBytes {
		drainExternalHTTPScoreResponseBody(resp.Body)
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

// drainExternalHTTPScoreResponseBody reads a bounded prefix of the response so
// HTTP/1.1 keep-alive can reuse the connection after non-success paths.
func drainExternalHTTPScoreResponseBody(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, externalHTTPScoreErrorBodyDrainBytes))
}
