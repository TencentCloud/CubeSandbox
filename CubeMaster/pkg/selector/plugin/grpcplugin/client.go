// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package grpcplugin implements external scheduler plugins over gRPC. It uses
// Unix Domain Sockets by default, performs a version/capability handshake,
// synchronizes immutable snapshots on a separate RPC, validates all plugin
// output, and bounds failures with timeouts and a small circuit breaker.
package grpcplugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const ProtocolVersion = "v1"

var (
	ErrCircuitOpen     = errors.New("external scheduler plugin circuit is open")
	ErrVersionMismatch = errors.New("external scheduler plugin snapshot version mismatch")
)

type breaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	openUntil time.Time
	halfOpen  bool
}

func (b *breaker) before() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.After(time.Now()) {
		return ErrCircuitOpen
	}
	if !b.openUntil.IsZero() {
		// Cooldown expired: enter half-open and allow a probe. before() admits
		// every caller once the cooldown has elapsed, so concurrent callers
		// would all become half-open probes (a single failed probe reopens
		// the circuit even if a concurrent probe succeeds). That is
		// acceptable today because every call path holds the client's syncMu,
		// which serializes probes; if call() is ever used without syncMu, add
		// a single-flight probe guard here.
		b.openUntil = time.Time{}
		b.failures = 0
		b.halfOpen = true
	}
	return nil
}

func (b *breaker) succeeded() {
	b.mu.Lock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.halfOpen = false
	b.mu.Unlock()
}

func (b *breaker) failed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.halfOpen {
		// A failed half-open probe means the plugin is still down: reopen
		// immediately instead of waiting for a fresh run of threshold
		// failures, so a dead plugin costs one timeout per cooldown window.
		b.halfOpen = false
		b.failures = 0
		b.openUntil = time.Now().Add(b.cooldown)
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.cooldown)
	}
}

type client struct {
	name       string
	timeout    time.Duration
	capability string
	connection *grpc.ClientConn
	rpc        schedulerplugin.SchedulerPluginClient
	breaker    breaker

	syncMu        sync.Mutex
	syncedVersion string
	closeOnce     sync.Once
}

func newClient(ctx context.Context, conf config.SchedulerProfilePluginConf, capability string) (*client, error) {
	name := strings.TrimSpace(conf.Name)
	if name == "" {
		return nil, errors.New("external scheduler plugin name is empty")
	}
	target := strings.TrimSpace(conf.SocketPath)
	if target == "" {
		return nil, fmt.Errorf("external scheduler plugin %q socket_path is empty", name)
	}
	dialTarget := target
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if strings.HasPrefix(target, "/") || strings.HasPrefix(target, "unix://") {
		path := strings.TrimPrefix(target, "unix://")
		dialTarget = "passthrough:///" + path
		options = append(options, grpc.WithContextDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialCtx, "unix", path)
		}))
	}
	connection, err := grpc.NewClient(dialTarget, options...)
	if err != nil {
		return nil, fmt.Errorf("dial external scheduler plugin %q: %w", name, err)
	}
	return newClientFromConn(ctx, conf, capability, connection)
}

func newClientFromConn(ctx context.Context, conf config.SchedulerProfilePluginConf, capability string, connection *grpc.ClientConn) (*client, error) {
	name := strings.TrimSpace(conf.Name)
	if name == "" {
		_ = connection.Close()
		return nil, errors.New("external scheduler plugin name is empty")
	}
	timeout := conf.Timeout
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	threshold := conf.CircuitBreakerFailures
	if threshold <= 0 {
		threshold = 3
	}
	cooldown := conf.CircuitBreakerCooldown
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	c := &client{
		name: name, timeout: timeout, capability: capability,
		connection: connection, rpc: schedulerplugin.NewSchedulerPluginClient(connection),
		breaker: breaker{threshold: threshold, cooldown: cooldown},
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := c.rpc.Handshake(handshakeCtx, &schedulerplugin.HandshakeRequest{
		ProtocolVersion: ProtocolVersion,
		PluginName:      name,
	})
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("external scheduler plugin %q handshake: %w", name, err)
	}
	if response.GetProtocolVersion() != ProtocolVersion {
		_ = connection.Close()
		return nil, fmt.Errorf("external scheduler plugin %q protocol version %q, want %q", name, response.GetProtocolVersion(), ProtocolVersion)
	}
	if response.GetPluginName() != "" && response.GetPluginName() != name {
		_ = connection.Close()
		return nil, fmt.Errorf("external scheduler plugin name %q, want %q", response.GetPluginName(), name)
	}
	if !slices.Contains(response.GetCapabilities(), capability) {
		_ = connection.Close()
		return nil, fmt.Errorf("external scheduler plugin %q does not advertise %q capability", name, capability)
	}
	return c, nil
}

func (c *client) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.connection.Close() })
	return err
}

func (c *client) call(ctx context.Context, invoke func(context.Context) error) error {
	if err := c.breaker.before(); err != nil {
		return fmt.Errorf("plugin %q: %w", c.name, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := invoke(callCtx); err != nil {
		// Count only plugin-attributable failures. When the parent context is
		// done, the cancellation came from the caller (the scheduling request
		// was aborted), not from the plugin — a burst of aborted requests must
		// not open the circuit against a healthy plugin. The plugin's own
		// slowness trips callCtx's deadline while the parent stays alive, so
		// it is still counted.
		if ctx.Err() == nil {
			c.breaker.failed()
		}
		return err
	}
	return nil
}

// reject marks a syntactically successful RPC with an invalid response as a
// plugin failure. Only a fully validated Filter or Score response resets the
// breaker; otherwise a successful snapshot sync could mask repeated failures
// in the actual scheduling RPC.
func (c *client) reject(err error) error {
	c.breaker.failed()
	return err
}

// syncSnapshot synchronizes the request's immutable snapshot with the plugin.
// The caller must hold c.syncMu; the lock is held across the whole
// sync+Filter/Score sequence by the Select methods below.
//
// Throughput ceiling: SnapshotVersion is unique per scheduling request, so
// the syncedVersion short-circuit can only deduplicate repeated syncs within
// a single request — across requests it always misses. Every scheduling
// request that reaches an external plugin therefore re-sends the complete
// pre-filter node pool, and syncMu serializes those requests (up to 2x
// timeout per plugin per request). Keying the snapshot by node-pool
// generation so unchanged pools are not re-sent is a deliberate follow-up.
func (c *client) syncSnapshot(selection *selctx.SelectorCtx) error {
	if selection.SnapshotVersion == "" {
		return errors.New("scheduler snapshot version is empty")
	}
	if c.syncedVersion == selection.SnapshotVersion {
		return nil
	}
	request := &schedulerplugin.SnapshotRequest{SnapshotVersion: selection.SnapshotVersion}
	snapshotNodes := selection.SnapshotNodes()
	request.Nodes = make([]*schedulerplugin.SnapshotNode, 0, len(snapshotNodes))
	for _, candidate := range snapshotNodes {
		request.Nodes = append(request.Nodes, snapshotNode(selection, candidate))
	}
	var response *schedulerplugin.SnapshotResponse
	if err := c.call(selection.Ctx, func(ctx context.Context) error {
		var err error
		response, err = c.rpc.SyncSnapshot(ctx, request)
		return err
	}); err != nil {
		return fmt.Errorf("external scheduler plugin %q sync snapshot: %w", c.name, err)
	}
	if response.GetSnapshotVersion() != selection.SnapshotVersion {
		return c.reject(fmt.Errorf("%w: plugin %q returned %q, want %q", ErrVersionMismatch, c.name, response.GetSnapshotVersion(), selection.SnapshotVersion))
	}
	c.syncedVersion = selection.SnapshotVersion
	return nil
}

func snapshotNode(selection *selctx.SelectorCtx, candidate *node.Node) *schedulerplugin.SnapshotNode {
	result := &schedulerplugin.SnapshotNode{
		Id: candidate.ID(), Ip: candidate.HostIP(), Healthy: candidate.Healthy,
		CpuTotal: int64(candidate.CpuTotal), CpuUtil: candidate.CpuUtil, CpuLoad: candidate.CpuLoadUsage,
		MemTotalMb: candidate.MemMBTotal, MemUsageMb: candidate.MemUsage,
		QuotaCpu: candidate.QuotaCpu, AllocatedCpu: candidate.QuotaCpuUsage,
		QuotaMemMb: candidate.QuotaMem, AllocatedMemMb: candidate.QuotaMemUsage,
		Creating: candidate.RealTimeCreateNum, LocalCreating: candidate.LocalCreateNum,
		MvmNum: candidate.MvmNum, SystemDiskSize: candidate.SystemDiskSize,
		DataDiskUsage: candidate.DataDiskUsagePer, StorageDiskUsage: candidate.StorageDiskUsagePer,
		SystemDiskUsage: candidate.SysDiskUsagePer,
		// Labels() hands out the live cached map; copy it before it goes on
		// the wire so an in-place label refresh can never race the marshal.
		Labels:         cloneMap(candidate.Labels()),
		LocalTemplates: append([]string(nil), candidate.LocalTemplates...),
	}
	if facts, ok := selection.SnapshotFacts(candidate.ID()); ok {
		result.TemplateLocal = facts.TemplateLocal
		result.SnapshotStorageWritable = facts.SnapshotStorageAllowed
	}
	return result
}

func requestContext(selection *selctx.SelectorCtx) *schedulerplugin.RequestContext {
	request := &schedulerplugin.RequestContext{
		InstanceType: selection.InstanceType,
		Labels:       cloneMap(selection.RequestLabels),
	}
	if resources := selection.GetReqRes(); resources != nil {
		request.CpuMillis = resources.Cpu.MilliValue()
		request.MemoryBytes = resources.Mem.Value()
		request.SystemDiskSize = resources.SystemDiskSize
		request.TemplateId = resources.TemplateID
	}
	return request
}

func cloneMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func candidateIndex(candidates node.NodeList) (map[string]*node.Node, []string, error) {
	byID := make(map[string]*node.Node, len(candidates))
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID() == "" {
			return nil, nil, errors.New("scheduler candidate has an empty id")
		}
		if _, exists := byID[candidate.ID()]; exists {
			return nil, nil, fmt.Errorf("duplicate scheduler candidate id %q", candidate.ID())
		}
		byID[candidate.ID()] = candidate
		ids = append(ids, candidate.ID())
	}
	return byID, ids, nil
}

type filterPlugin struct{ client *client }

func NewFilter(ctx context.Context, conf config.SchedulerProfilePluginConf) (filter.Selector, error) {
	client, err := newClient(ctx, conf, "filter")
	if err != nil {
		return nil, err
	}
	return &filterPlugin{client: client}, nil
}

func (p *filterPlugin) ID() string   { return "filter/grpc/" + p.client.name }
func (p *filterPlugin) Close() error { return p.client.Close() }

func (p *filterPlugin) Select(selection *selctx.SelectorCtx) (node.NodeList, error) {
	// Snapshot versions are unique per scheduling request, while the plugin
	// tracks a single synced version. Holding syncMu across the whole
	// sync+Filter sequence keeps the synced version valid until the Filter RPC
	// is issued; this intentionally serializes concurrent requests sharing
	// this client.
	p.client.syncMu.Lock()
	defer p.client.syncMu.Unlock()
	if err := p.client.syncSnapshot(selection); err != nil {
		return nil, err
	}
	candidates := selection.Nodes()
	byID, ids, err := candidateIndex(candidates)
	if err != nil {
		return nil, err
	}
	request := &schedulerplugin.FilterRequest{
		SnapshotVersion: selection.SnapshotVersion,
		Request:         requestContext(selection),
		CandidateIds:    ids,
	}
	var response *schedulerplugin.FilterResponse
	if err := p.client.call(selection.Ctx, func(ctx context.Context) error {
		var err error
		response, err = p.client.rpc.Filter(ctx, request)
		return err
	}); err != nil {
		return nil, fmt.Errorf("external scheduler filter %q: %w", p.client.name, err)
	}
	if response.GetSnapshotVersion() != selection.SnapshotVersion {
		return nil, p.client.reject(fmt.Errorf("%w: plugin %q returned %q, want %q", ErrVersionMismatch, p.client.name, response.GetSnapshotVersion(), selection.SnapshotVersion))
	}
	kept := make(map[string]struct{}, len(response.GetKeptIds()))
	for _, id := range response.GetKeptIds() {
		if _, exists := byID[id]; !exists {
			return nil, p.client.reject(fmt.Errorf("external scheduler filter %q returned non-candidate node %q", p.client.name, id))
		}
		if _, duplicate := kept[id]; duplicate {
			return nil, p.client.reject(fmt.Errorf("external scheduler filter %q returned duplicate node %q", p.client.name, id))
		}
		kept[id] = struct{}{}
	}
	result := make(node.NodeList, 0, len(kept))
	for _, candidate := range candidates {
		if _, ok := kept[candidate.ID()]; ok {
			result = append(result, candidate)
		}
	}
	p.client.breaker.succeeded()
	return result, nil
}

type scorePlugin struct {
	client *client
	weight float64
}

func NewScore(ctx context.Context, conf config.SchedulerProfilePluginConf) (score.Selector, error) {
	client, err := newClient(ctx, conf, "score")
	if err != nil {
		return nil, err
	}
	weight := conf.Weight
	if weight == 0 {
		// Zero means "unset" and defaults to 1; an explicit weight: 0 is
		// indistinguishable from unset and therefore unsupported. A zero-weight
		// score would contribute nothing to the weighted total anyway —
		// operators disable a score via the profile instead.
		weight = 1
	}
	return &scorePlugin{client: client, weight: weight}, nil
}

func (p *scorePlugin) ID() string      { return "score/grpc/" + p.client.name }
func (p *scorePlugin) Weight() float64 { return p.weight }
func (p *scorePlugin) Disable() bool   { return false }
func (p *scorePlugin) Close() error    { return p.client.Close() }

func (p *scorePlugin) Select(selection *selctx.SelectorCtx) (node.NodeScoreList, error) {
	// See filterPlugin.Select: syncMu is held across the whole sync+Score
	// sequence, intentionally serializing concurrent requests sharing this
	// client so the synced snapshot version stays valid for the Score RPC.
	p.client.syncMu.Lock()
	defer p.client.syncMu.Unlock()
	if err := p.client.syncSnapshot(selection); err != nil {
		return nil, err
	}
	candidates := selection.Nodes()
	byID, ids, err := candidateIndex(candidates)
	if err != nil {
		return nil, err
	}
	request := &schedulerplugin.ScoreRequest{
		SnapshotVersion: selection.SnapshotVersion,
		Request:         requestContext(selection),
		CandidateIds:    ids,
	}
	var response *schedulerplugin.ScoreResponse
	if err := p.client.call(selection.Ctx, func(ctx context.Context) error {
		var err error
		response, err = p.client.rpc.Score(ctx, request)
		return err
	}); err != nil {
		return nil, fmt.Errorf("external scheduler score %q: %w", p.client.name, err)
	}
	if response.GetSnapshotVersion() != selection.SnapshotVersion {
		return nil, p.client.reject(fmt.Errorf("%w: plugin %q returned %q, want %q", ErrVersionMismatch, p.client.name, response.GetSnapshotVersion(), selection.SnapshotVersion))
	}
	values := make(map[string]float64, len(response.GetScores()))
	for _, item := range response.GetScores() {
		id, value := item.GetNodeId(), item.GetScore()
		if _, exists := byID[id]; !exists {
			return nil, p.client.reject(fmt.Errorf("external scheduler score %q returned non-candidate node %q", p.client.name, id))
		}
		if _, duplicate := values[id]; duplicate {
			return nil, p.client.reject(fmt.Errorf("external scheduler score %q returned duplicate node %q", p.client.name, id))
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return nil, p.client.reject(fmt.Errorf("external scheduler score %q returned %v for node %q outside [0,100]", p.client.name, value, id))
		}
		values[id] = value
	}
	if len(values) != len(candidates) {
		return nil, p.client.reject(fmt.Errorf("external scheduler score %q returned %d scores for %d candidates", p.client.name, len(values), len(candidates)))
	}
	result := make(node.NodeScoreList, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, &node.NodeScore{
			InsID: candidate.ID(), Score: values[candidate.ID()], MvmNum: candidate.MvmNum, OrigNode: candidate,
		})
	}
	p.client.breaker.succeeded()
	return result, nil
}
