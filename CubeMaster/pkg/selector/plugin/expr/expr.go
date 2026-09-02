// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package expr implements stateless CEL scheduler Filter and Score plugins.
// Expressions are compiled once during profile activation and programs are
// safe for concurrent evaluation.
package expr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
)

type evaluator struct {
	name    string
	program cel.Program
	weight  float64
}

func compile(conf config.SchedulerProfilePluginConf, wantFilter bool) (*evaluator, error) {
	name := strings.TrimSpace(conf.Name)
	if name == "" {
		return nil, errors.New("expression plugin name is empty")
	}
	expression := strings.TrimSpace(conf.Expr)
	if expression == "" {
		return nil, fmt.Errorf("expression plugin %q has an empty expr", name)
	}
	if len(expression) > 4096 {
		return nil, fmt.Errorf("expression plugin %q exceeds the 4096-byte limit", name)
	}
	env, err := cel.NewEnv(
		cel.Types(&schedulerplugin.SnapshotNode{}, &schedulerplugin.RequestContext{}),
		cel.Variable("node", cel.ObjectType("cube.schedulerplugin.v1.SnapshotNode")),
		cel.Variable("request", cel.ObjectType("cube.schedulerplugin.v1.RequestContext")),
	)
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile expression plugin %q: %w", name, issues.Err())
	}
	output := ast.OutputType()
	if wantFilter {
		if output != cel.BoolType {
			return nil, fmt.Errorf("filter expression plugin %q returns %s, want bool", name, output)
		}
	} else if output != cel.DoubleType && output != cel.IntType && output != cel.UintType && output != cel.DynType {
		return nil, fmt.Errorf("score expression plugin %q returns %s, want number", name, output)
	}
	program, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(10_000))
	if err != nil {
		return nil, fmt.Errorf("create expression plugin %q program: %w", name, err)
	}
	weight := conf.Weight
	if weight == 0 {
		// Zero means "unset" and defaults to 1; an explicit weight: 0 is
		// indistinguishable from unset and therefore unsupported. A
		// zero-weight score would contribute nothing to the weighted total
		// anyway — operators disable a score via the profile instead.
		weight = 1
	}
	return &evaluator{name: name, program: program, weight: weight}, nil
}

type filterPlugin struct{ *evaluator }

func NewFilter(_ context.Context, conf config.SchedulerProfilePluginConf) (filter.Selector, error) {
	evaluator, err := compile(conf, true)
	if err != nil {
		return nil, err
	}
	return &filterPlugin{evaluator: evaluator}, nil
}

func (p *filterPlugin) ID() string { return "filter/expr/" + p.name }

func (p *filterPlugin) Select(selection *selctx.SelectorCtx) (node.NodeList, error) {
	candidates := selection.Nodes()
	kept := make(node.NodeList, 0, len(candidates))
	for _, candidate := range candidates {
		// Fail closed on malformed candidates, matching the gRPC plugin's
		// candidateIndex contract.
		if candidate == nil || candidate.ID() == "" {
			return nil, fmt.Errorf("expression filter %q: scheduler candidate has an empty id", p.name)
		}
		out, _, err := p.program.ContextEval(selection.Ctx, activation(selection, candidate))
		if err != nil {
			return nil, fmt.Errorf("expression filter %q on node %q: %w", p.name, candidate.ID(), err)
		}
		value, ok := out.Value().(bool)
		if !ok {
			return nil, fmt.Errorf("expression filter %q returned %T, want bool", p.name, out.Value())
		}
		if value {
			kept = append(kept, candidate)
		}
	}
	return kept, nil
}

type scorePlugin struct{ *evaluator }

func NewScore(_ context.Context, conf config.SchedulerProfilePluginConf) (score.Selector, error) {
	evaluator, err := compile(conf, false)
	if err != nil {
		return nil, err
	}
	return &scorePlugin{evaluator: evaluator}, nil
}

func (p *scorePlugin) ID() string      { return "score/expr/" + p.name }
func (p *scorePlugin) Weight() float64 { return p.weight }
func (p *scorePlugin) Disable() bool   { return false }

// Select evaluates the expression once per candidate. The expression must be
// a TOTAL function over node states: any per-node evaluation error, non-number
// result, or value outside [0,100] fails the whole scheduling request
// (fail-closed — a partially scored candidate set would silently skew
// placement). Operator-authored expressions must therefore clamp or guard
// every node state they may meet (e.g. wrap divisions, bound `100 -
// node.cpu_util` against cpu_util > 100). The profile runner in the
// integration half can degrade a failing score plugin to its configured
// default_score instead of aborting; the plugin itself stays strict.
func (p *scorePlugin) Select(selection *selctx.SelectorCtx) (node.NodeScoreList, error) {
	candidates := selection.Nodes()
	result := make(node.NodeScoreList, 0, len(candidates))
	for _, candidate := range candidates {
		// Fail closed on malformed candidates, matching the gRPC plugin's
		// candidateIndex contract.
		if candidate == nil || candidate.ID() == "" {
			return nil, fmt.Errorf("expression score %q: scheduler candidate has an empty id", p.name)
		}
		out, _, err := p.program.ContextEval(selection.Ctx, activation(selection, candidate))
		if err != nil {
			return nil, fmt.Errorf("expression score %q on node %q: %w", p.name, candidate.ID(), err)
		}
		value, err := number(out.Value())
		if err != nil {
			return nil, fmt.Errorf("expression score %q on node %q: %w", p.name, candidate.ID(), err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return nil, fmt.Errorf("expression score %q on node %q returned %v outside [0,100]", p.name, candidate.ID(), value)
		}
		result = append(result, &node.NodeScore{InsID: candidate.ID(), Score: value, MvmNum: candidate.MvmNum, OrigNode: candidate})
	}
	return result, nil
}

func number(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case int64:
		return float64(typed), nil
	case uint64:
		return float64(typed), nil
	default:
		return 0, fmt.Errorf("returned %T, want number", value)
	}
}

func activation(selection *selctx.SelectorCtx, candidate *node.Node) map[string]any {
	requestValues := &schedulerplugin.RequestContext{
		InstanceType: selection.InstanceType,
		Labels:       cloneStringMap(selection.RequestLabels),
	}
	request := selection.GetReqRes()
	if request != nil {
		requestValues.CpuMillis = request.Cpu.MilliValue()
		requestValues.MemoryBytes = request.Mem.Value()
		requestValues.SystemDiskSize = request.SystemDiskSize
		requestValues.TemplateId = request.TemplateID
	}
	localTemplates := append([]string(nil), candidate.LocalTemplates...)
	templateLocal := false
	if facts, ok := selection.SnapshotFacts(candidate.ID()); ok && facts.TemplateLocalKnown {
		templateLocal = facts.TemplateLocal
	}
	if request != nil && request.TemplateID != "" {
		if facts, ok := selection.SnapshotFacts(candidate.ID()); !ok || !facts.TemplateLocalKnown {
			for _, templateID := range localTemplates {
				if templateID == request.TemplateID {
					templateLocal = true
					break
				}
			}
		}
	}
	nodeValues := &schedulerplugin.SnapshotNode{
		Id: candidate.ID(), Ip: candidate.HostIP(), Healthy: candidate.Healthy,
		CpuTotal: int64(candidate.CpuTotal), CpuUtil: candidate.CpuUtil, CpuLoad: candidate.CpuLoadUsage,
		MemTotalMb: candidate.MemMBTotal, MemUsageMb: candidate.MemUsage,
		QuotaCpu: candidate.QuotaCpu, AllocatedCpu: candidate.QuotaCpuUsage,
		QuotaMemMb: candidate.QuotaMem, AllocatedMemMb: candidate.QuotaMemUsage,
		Creating: candidate.RealTimeCreateNum, LocalCreating: candidate.LocalCreateNum,
		MvmNum: candidate.MvmNum, SystemDiskSize: candidate.SystemDiskSize,
		DataDiskUsage: candidate.DataDiskUsagePer, StorageDiskUsage: candidate.StorageDiskUsagePer,
		SystemDiskUsage: candidate.SysDiskUsagePer, Labels: cloneStringMap(candidate.Labels()),
		LocalTemplates: localTemplates, TemplateLocal: templateLocal,
	}
	if facts, ok := selection.SnapshotFacts(candidate.ID()); ok && facts.SnapshotStorageKnown {
		// Storage writability is exposed only when the runner actually
		// evaluated it; unknown is indistinguishable from absent and stays
		// false (fail closed).
		nodeValues.SnapshotStorageWritable = facts.SnapshotStorageAllowed
	}
	return map[string]any{"node": nodeValues, "request": requestValues}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
