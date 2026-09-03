// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package profile validates scheduler profile configuration, compiles plugin
// pipelines and routes requests by instance type and an allow-listed label set.
package profile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

type FilterFailurePolicy string
type ScoreFailurePolicy string
type NoCandidatePolicy string

const (
	FilterFailClosed FilterFailurePolicy = "fail-closed"
	FilterFailOpen   FilterFailurePolicy = "fail-open"

	ScoreDefaultScore ScoreFailurePolicy = "default-score"
	ScoreFailClosed   ScoreFailurePolicy = "fail-closed"
	ScoreSkip         ScoreFailurePolicy = "skip" // legacy compatibility only

	NoCandidateFail    NoCandidatePolicy = "fail"
	NoCandidateBackoff NoCandidatePolicy = "backoff"

	SelectionRandom  = "random"
	SelectionSpread  = "spread"
	SelectionHighest = "highest"
)

var mandatoryGuardNames = []string{"node_safety", "cpu", "mem", "disk", "template_locality", "realtime_create_num"}

type FilterPlugin struct {
	Name     string
	Selector filter.Selector
	Failure  FilterFailurePolicy
}

type ScorePlugin struct {
	Name         string
	Selector     score.Selector
	Weight       float64
	Failure      ScoreFailurePolicy
	DefaultScore float64
	ForceEnabled bool
	// AllowEmpty treats an empty score list as "not applicable" and skips the
	// plugin instead of failing the completeness check. It is set for
	// built-in go scorers (e.g. affinity_score, image_score) that legitimately
	// no-op when a request is outside their scope, mirroring the legacy path.
	AllowEmpty bool
}

type Pipeline struct {
	Name        string
	Guards      []FilterPlugin
	Filters     []FilterPlugin
	Scores      []ScorePlugin
	TopN        int
	Selection   string
	NoCandidate NoCandidatePolicy
}

type compiledRoute struct {
	instanceTypes []*regexp.Regexp
	labels        map[string]string
	pipeline      *Pipeline
}

type Set struct {
	routes   []compiledRoute
	fallback *Pipeline
	closers  []io.Closer

	lifecycleMu sync.Mutex
	references  int
	retired     bool
	closeOnce   sync.Once
}

func Compile(ctx context.Context, cfg *config.Config, registry *plugin.Registry) (_ *Set, err error) {
	if cfg == nil || cfg.Scheduler == nil {
		return nil, errors.New("scheduler config is nil")
	}
	if registry == nil {
		return nil, errors.New("scheduler plugin registry is nil")
	}
	set := &Set{}
	defer func() {
		if err != nil {
			_ = set.Close()
		}
	}()
	if len(cfg.Scheduler.Profiles) == 0 {
		legacy, compileErr := compileLegacy(ctx, cfg.Scheduler, registry, set)
		if compileErr != nil {
			return nil, fmt.Errorf("compile legacy scheduler profile: %w", compileErr)
		}
		set.fallback = legacy
		return set, nil
	}

	allowedLabels := make(map[string]struct{}, len(cfg.Scheduler.ProfileRouteLabelKeys))
	for _, key := range cfg.Scheduler.ProfileRouteLabelKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, errors.New("scheduler profile_route_label_keys contains an empty key")
		}
		if _, duplicate := allowedLabels[key]; duplicate {
			return nil, fmt.Errorf("duplicate scheduler profile route label key %q", key)
		}
		allowedLabels[key] = struct{}{}
	}

	seenNames := make(map[string]struct{}, len(cfg.Scheduler.Profiles))
	defaultSeen := false
	defaultName := ""
	for index := range cfg.Scheduler.Profiles {
		profileConf := cfg.Scheduler.Profiles[index]
		name := strings.TrimSpace(profileConf.Name)
		if name == "" {
			return nil, fmt.Errorf("scheduler profile at index %d has an empty name", index)
		}
		nameKey := strings.ToLower(name)
		if _, duplicate := seenNames[nameKey]; duplicate {
			return nil, fmt.Errorf("duplicate scheduler profile name %q", name)
		}
		seenNames[nameKey] = struct{}{}
		pipeline, compileErr := compileProfile(ctx, profileConf, registry, set)
		if compileErr != nil {
			return nil, fmt.Errorf("compile scheduler profile %q: %w", name, compileErr)
		}
		if profileConf.Selection.TopN == 0 && cfg.Scheduler.PrioritySelectNum != 1 {
			// Migration trap: a selection-less profile defaults to top_n 1
			// (deterministic best-node pick), silently replacing the legacy
			// spread — both top-N (priority_select_num > 1) and whole-pool
			// (priority_select_num unset, normalized to -1) spreads are
			// affected. Nothing fails, so say it at compile time.
			log.G(ctx).Warnf("RISK: scheduler profile %q sets no selection.top_n and defaults to 1 (best-node pick) "+
				"while legacy priority_select_num=%d (>1 means top-N spread, -1 means whole-pool spread); "+
				"set selection.top_n explicitly to keep spreading",
				name, cfg.Scheduler.PrioritySelectNum)
		}
		if profileConf.Default {
			if defaultSeen {
				return nil, errors.New("multiple default scheduler profiles configured")
			}
			if len(profileConf.Route.InstanceTypes) != 0 || len(profileConf.Route.Labels) != 0 {
				return nil, fmt.Errorf("default scheduler profile %q must not define a route", name)
			}
			defaultSeen = true
			defaultName = name
			set.fallback = pipeline
			continue
		}
		route, compileErr := compileRoute(profileConf.Route, allowedLabels, pipeline)
		if compileErr != nil {
			return nil, fmt.Errorf("compile scheduler profile %q route: %w", name, compileErr)
		}
		set.routes = append(set.routes, route)
	}
	if !defaultSeen {
		legacy, compileErr := compileLegacy(ctx, cfg.Scheduler, registry, set)
		if compileErr != nil {
			return nil, fmt.Errorf("compile legacy scheduler profile: %w", compileErr)
		}
		if len(cfg.Scheduler.Profiles) > 0 {
			// Unmatched traffic falls back to this legacy pipeline; when no
			// legacy enable_filters/enable_scorers are configured it compiles
			// to an unguarded random selection, so a missing default profile
			// silently strips the safety guards from unmatched requests.
			log.G(ctx).Warnf("RISK: scheduler profiles configured without a default profile; "+
				"requests matching no route fall back to the legacy pipeline (%d filters, %d scorers compiled from enable_filters/enable_scorers)",
				len(legacy.Filters), len(legacy.Scores))
		}
		set.fallback = legacy
	} else {
		// The default profile catches every request that matches no route —
		// including migrate/restore flows, which carry no routing labels and
		// whose restore placement may arrive with an empty resource spec. The
		// mandatory resource guards never back off, so on a packed cluster
		// such a request hard-fails with no-candidate where the pre-profile
		// code would have rescued it via BackoffSelect. Operators who must
		// keep those flows schedulable should route them explicitly.
		log.G(ctx).Warnf("RISK: default scheduler profile %q catches all unmatched traffic, including spec-less "+
			"migrate/restore placements; its mandatory resource guards never back off, so such requests can "+
			"hard-fail with no BackoffSelect escape", defaultName)
	}
	return set, nil
}

func compileLegacy(ctx context.Context, scheduler *config.WrapperSchedulerConf, registry *plugin.Registry, set *Set) (*Pipeline, error) {
	pipeline := &Pipeline{
		Name: "default", TopN: scheduler.PrioritySelectNum, Selection: SelectionRandom,
		NoCandidate: NoCandidateBackoff,
	}
	if scheduler.Filter != nil {
		for _, name := range scheduler.Filter.EnableFilters {
			conf := config.SchedulerProfilePluginConf{Name: name, Type: plugin.TypeGo}
			selector, err := registry.BuildFilter(ctx, conf)
			if err != nil {
				return nil, fmt.Errorf("enable_filters entry %q is not a registered plugin; remove it from the config or register the plugin before starting CubeMaster: %w", name, err)
			}
			pipeline.Filters = append(pipeline.Filters, FilterPlugin{Name: name, Selector: selector, Failure: FilterFailClosed})
			set.addCloser(selector)
		}
	}
	if scheduler.Score != nil && scheduler.Score.ResourceWeights != nil {
		for _, name := range scheduler.Score.EnableScorers {
			conf := config.SchedulerProfilePluginConf{Name: name, Type: plugin.TypeGo}
			selector, err := registry.BuildScore(ctx, conf)
			if err != nil {
				return nil, fmt.Errorf("enable_scorers entry %q is not a registered plugin; remove it from the config or register the plugin before starting CubeMaster: %w", name, err)
			}
			weight := selector.Weight()
			if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
				// The pre-profile runScoreFilter tolerated a non-positive
				// weight (the scorer contributed score*0 and added nothing
				// to the normalizer). Skipping it here keeps such configs
				// scheduling instead of failing every Select at runtime.
				log.G(ctx).Warnf("enable_scorers entry %q has invalid weight %v; skipping the scorer", name, weight)
				if closer, ok := selector.(io.Closer); ok {
					_ = closer.Close()
				}
				continue
			}
			pipeline.Scores = append(pipeline.Scores, ScorePlugin{
				Name: name, Selector: selector, Weight: weight, Failure: ScoreSkip,
			})
			set.addCloser(selector)
		}
	}
	return pipeline, nil
}

func compileProfile(ctx context.Context, conf config.SchedulerProfileConf, registry *plugin.Registry, set *Set) (*Pipeline, error) {
	filterFailure, scoreFailure, noCandidate, err := failurePolicies(conf.Failure)
	if err != nil {
		return nil, err
	}
	topN := conf.Selection.TopN
	if topN == 0 {
		topN = 1
	}
	if topN < -1 {
		return nil, fmt.Errorf("selection.top_n must be -1 or positive, got %d", topN)
	}
	method := strings.ToLower(strings.TrimSpace(conf.Selection.Method))
	if method == "" {
		method = SelectionRandom
	}
	switch method {
	case SelectionRandom, SelectionSpread, SelectionHighest:
	default:
		return nil, fmt.Errorf("unknown selection.method %q", method)
	}
	pipeline := &Pipeline{
		Name: strings.TrimSpace(conf.Name), TopN: topN, Selection: method, NoCandidate: noCandidate,
	}
	for _, name := range mandatoryGuardNames {
		selector, buildErr := registry.BuildFilter(ctx, config.SchedulerProfilePluginConf{Name: name, Type: plugin.TypeGo})
		if buildErr != nil {
			return nil, fmt.Errorf("build mandatory guard %q: %w", name, buildErr)
		}
		pipeline.Guards = append(pipeline.Guards, FilterPlugin{Name: name, Selector: selector, Failure: FilterFailClosed})
		set.addCloser(selector)
	}
	guardSet := make(map[string]struct{}, len(mandatoryGuardNames))
	for _, name := range mandatoryGuardNames {
		guardSet[name] = struct{}{}
	}
	seenFilters := make(map[string]struct{}, len(conf.Filters))
	for _, pluginConf := range conf.Filters {
		if !enabled(pluginConf.Enabled) {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(pluginConf.Name))
		if _, guard := guardSet[name]; guard {
			return nil, fmt.Errorf("mandatory guard %q must not be configured as an optional filter", name)
		}
		key := profilePluginKey(pluginConf)
		if _, duplicate := seenFilters[key]; duplicate {
			return nil, fmt.Errorf("duplicate filter plugin %q", key)
		}
		seenFilters[key] = struct{}{}
		selector, buildErr := registry.BuildFilter(ctx, pluginConf)
		if buildErr != nil {
			return nil, buildErr
		}
		pipeline.Filters = append(pipeline.Filters, FilterPlugin{Name: name, Selector: selector, Failure: filterFailure})
		set.addCloser(selector)
	}
	seenScores := make(map[string]struct{}, len(conf.Scores))
	for _, pluginConf := range conf.Scores {
		if !enabled(pluginConf.Enabled) {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(pluginConf.Name))
		key := profilePluginKey(pluginConf)
		if _, duplicate := seenScores[key]; duplicate {
			return nil, fmt.Errorf("duplicate score plugin %q", key)
		}
		seenScores[key] = struct{}{}
		selector, buildErr := registry.BuildScore(ctx, pluginConf)
		if buildErr != nil {
			return nil, buildErr
		}
		weight := pluginConf.Weight
		if weight == 0 {
			weight = selector.Weight()
		}
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return nil, fmt.Errorf("score plugin %q has invalid weight %v", name, weight)
		}
		if pluginConf.DefaultScore < 0 || pluginConf.DefaultScore > 100 || math.IsNaN(pluginConf.DefaultScore) || math.IsInf(pluginConf.DefaultScore, 0) {
			return nil, fmt.Errorf("score plugin %q has invalid default_score %v", name, pluginConf.DefaultScore)
		}
		pipeline.Scores = append(pipeline.Scores, ScorePlugin{
			Name: name, Selector: selector, Weight: weight, Failure: scoreFailure,
			DefaultScore: pluginConf.DefaultScore, ForceEnabled: true,
			// Empty-result tolerance ("not applicable, skip") is curated for
			// the in-tree go built-ins only — by kind AND name: an expr/grpc
			// scorer configured under a curated name, or any third-party go
			// scorer, must trip the failure policy on an empty result rather
			// than silently degrade the ranking to candidate order.
			AllowEmpty: profilePluginKind(pluginConf) == plugin.TypeGo && emptyTolerantBuiltinScores[name],
		})
		set.addCloser(selector)
	}
	return pipeline, nil
}

func failurePolicies(conf config.SchedulerFailureConf) (FilterFailurePolicy, ScoreFailurePolicy, NoCandidatePolicy, error) {
	filterPolicy := FilterFailurePolicy(strings.ToLower(strings.TrimSpace(conf.Filter)))
	if filterPolicy == "" {
		filterPolicy = FilterFailClosed
	}
	if filterPolicy != FilterFailClosed && filterPolicy != FilterFailOpen {
		return "", "", "", fmt.Errorf("unknown failure.filter policy %q", conf.Filter)
	}
	scorePolicy := ScoreFailurePolicy(strings.ToLower(strings.TrimSpace(conf.Score)))
	if scorePolicy == "" {
		scorePolicy = ScoreDefaultScore
	}
	if scorePolicy != ScoreDefaultScore && scorePolicy != ScoreFailClosed {
		return "", "", "", fmt.Errorf("unknown failure.score policy %q", conf.Score)
	}
	noCandidate := NoCandidatePolicy(strings.ToLower(strings.TrimSpace(conf.NoCandidate)))
	if noCandidate == "" {
		noCandidate = NoCandidateFail
	}
	if noCandidate != NoCandidateFail && noCandidate != NoCandidateBackoff {
		return "", "", "", fmt.Errorf("unknown failure.no_candidate policy %q", conf.NoCandidate)
	}
	return filterPolicy, scorePolicy, noCandidate, nil
}

func enabled(value *bool) bool { return value == nil || *value }

// emptyTolerantBuiltinScores are the in-tree go scorers that legitimately
// return an empty result when a request is outside their scope:
// affinity_score without node-preference affinity, image_score without
// applicable resource weights. Membership — not the plugin's type — decides
// empty-result tolerance.
var emptyTolerantBuiltinScores = map[string]bool{
	"affinity_score": true,
	"image_score":    true,
}

func profilePluginKind(conf config.SchedulerProfilePluginConf) string {
	kind := strings.ToLower(strings.TrimSpace(conf.Type))
	if kind == "" || kind == "builtin" {
		kind = plugin.TypeGo
	}
	return kind
}

func profilePluginKey(conf config.SchedulerProfilePluginConf) string {
	return profilePluginKind(conf) + "/" + strings.ToLower(strings.TrimSpace(conf.Name))
}

func compileRoute(conf config.SchedulerProfileRouteConf, allowedLabels map[string]struct{}, pipeline *Pipeline) (compiledRoute, error) {
	if len(conf.InstanceTypes) == 0 && len(conf.Labels) == 0 {
		return compiledRoute{}, errors.New("non-default profile route is empty")
	}
	route := compiledRoute{labels: make(map[string]string, len(conf.Labels)), pipeline: pipeline}
	for _, expression := range conf.InstanceTypes {
		expression = strings.TrimSpace(expression)
		if expression == "" {
			return compiledRoute{}, errors.New("route.instance_types contains an empty expression")
		}
		pattern, err := regexp.Compile("^(?:" + expression + ")$")
		if err != nil {
			return compiledRoute{}, fmt.Errorf("invalid instance type expression %q: %w", expression, err)
		}
		route.instanceTypes = append(route.instanceTypes, pattern)
	}
	for key, value := range conf.Labels {
		if _, allowed := allowedLabels[key]; !allowed {
			return compiledRoute{}, fmt.Errorf("route label %q is not in profile_route_label_keys", key)
		}
		if value == "" {
			// matches() compares against selection.RequestLabels[key], which
			// is "" when the request carries no such label — an empty route
			// value would silently match those requests (including
			// migrate/restore paths that never populate RequestLabels).
			return compiledRoute{}, fmt.Errorf("route label %q has an empty value", key)
		}
		route.labels[key] = value
	}
	return route, nil
}

func (r compiledRoute) matches(selection *selctx.SelectorCtx) bool {
	if len(r.instanceTypes) > 0 {
		matched := false
		for _, pattern := range r.instanceTypes {
			if pattern.MatchString(selection.InstanceType) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for key, expected := range r.labels {
		if selection.RequestLabels[key] != expected {
			return false
		}
	}
	return true
}

func (s *Set) Match(selection *selctx.SelectorCtx) *Pipeline {
	if s == nil {
		return nil
	}
	for _, route := range s.routes {
		if route.matches(selection) {
			return route.pipeline
		}
	}
	return s.fallback
}

// Acquire pins one immutable Profile set for the lifetime of a scheduling
// request. Once a set is retired by hot reload it rejects new leases, while
// existing requests may finish before external plugin connections are closed.
func (s *Set) Acquire(selection *selctx.SelectorCtx) (*Pipeline, func(), bool) {
	if s == nil {
		return nil, nil, false
	}
	s.lifecycleMu.Lock()
	if s.retired {
		s.lifecycleMu.Unlock()
		return nil, nil, false
	}
	s.references++
	s.lifecycleMu.Unlock()

	var once sync.Once
	release := func() { once.Do(s.release) }
	return s.Match(selection), release, true
}

func (s *Set) release() {
	s.lifecycleMu.Lock()
	if s.references > 0 {
		s.references--
	}
	closeNow := s.retired && s.references == 0
	s.lifecycleMu.Unlock()
	if closeNow {
		_ = s.closePlugins()
	}
}

func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.routes)+1)
	for _, route := range s.routes {
		names = append(names, route.pipeline.Name)
	}
	if s.fallback != nil {
		names = append(names, s.fallback.Name)
	}
	sort.Strings(names)
	return names
}

// UsesScore reports whether any compiled pipeline in the set binds the named
// score plugin (case-insensitive), the legacy fallback included. The
// scheduler uses it to keep the multi_factor_weighted_average background
// refresher inert while no pipeline consumes the node scores it maintains.
func (s *Set) UsesScore(name string) bool {
	if s == nil {
		return false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	used := func(p *Pipeline) bool {
		if p == nil {
			return false
		}
		for _, sc := range p.Scores {
			if strings.ToLower(sc.Name) == name {
				return true
			}
		}
		return false
	}
	for _, route := range s.routes {
		if used(route.pipeline) {
			return true
		}
	}
	return used(s.fallback)
}

func (s *Set) addCloser(value any) {
	if closer, ok := value.(io.Closer); ok {
		s.closers = append(s.closers, closer)
	}
}

func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	s.retired = true
	closeNow := s.references == 0
	s.lifecycleMu.Unlock()
	if !closeNow {
		return nil
	}
	return s.closePlugins()
}

func (s *Set) closePlugins() error {
	var result error
	s.closeOnce.Do(func() {
		for _, closer := range s.closers {
			if err := closer.Close(); err != nil {
				result = errors.Join(result, err)
			}
		}
	})
	return result
}
