// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package plugin contains the unified scheduler plugin registry. Built-in Go,
// CEL expression and external gRPC plugins are resolved through the same
// factory API and are validated before a profile becomes active.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

const (
	TypeGo         = "go"
	TypeExpression = "expr"
	TypeGRPC       = "grpc"
)

var (
	ErrDuplicateRegistration = errors.New("scheduler plugin already registered")
	ErrUnknownPlugin         = errors.New("unknown scheduler plugin")
	ErrUnknownPluginType     = errors.New("unknown scheduler plugin type")
)

type FilterFactory func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error)
type ScoreFactory func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error)

// Registry keeps phase-specific factories under one namespace. Named
// factories are used by compiled Go plugins; providers create dynamically
// named plugins such as CEL expressions and gRPC clients.
type Registry struct {
	mu sync.RWMutex

	filters         map[string]map[string]FilterFactory
	scores          map[string]map[string]ScoreFactory
	filterProviders map[string]FilterFactory
	scoreProviders  map[string]ScoreFactory
}

var goExtensions = NewRegistry()

// RegisterGoFilter and RegisterGoScore are the code-level extension points for
// in-process plugins. A plugin package normally calls them from init(), and the
// CubeMaster binary imports that package (usually with a blank import).
func RegisterGoFilter(name string, factory FilterFactory) error {
	return goExtensions.RegisterFilter(TypeGo, name, factory)
}

func RegisterGoScore(name string, factory ScoreFactory) error {
	return goExtensions.RegisterScore(TypeGo, name, factory)
}

// ApplyGoExtensions copies all process-level Go registrations into an isolated
// scheduler registry. Duplicate names are rejected before activation.
func ApplyGoExtensions(target *Registry) error {
	if target == nil {
		return errors.New("target scheduler plugin registry is nil")
	}
	goExtensions.mu.RLock()
	filters := make(map[string]FilterFactory, len(goExtensions.filters[TypeGo]))
	for name, factory := range goExtensions.filters[TypeGo] {
		filters[name] = factory
	}
	scores := make(map[string]ScoreFactory, len(goExtensions.scores[TypeGo]))
	for name, factory := range goExtensions.scores[TypeGo] {
		scores[name] = factory
	}
	goExtensions.mu.RUnlock()
	for name, factory := range filters {
		if err := target.RegisterFilter(TypeGo, name, factory); err != nil {
			return err
		}
	}
	for name, factory := range scores {
		if err := target.RegisterScore(TypeGo, name, factory); err != nil {
			return err
		}
	}
	return nil
}

func NewRegistry() *Registry {
	return &Registry{
		filters:         make(map[string]map[string]FilterFactory),
		scores:          make(map[string]map[string]ScoreFactory),
		filterProviders: make(map[string]FilterFactory),
		scoreProviders:  make(map[string]ScoreFactory),
	}
}

func normalizeType(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" || kind == "builtin" {
		return TypeGo
	}
	return kind
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (r *Registry) RegisterFilter(kind, name string, factory FilterFactory) error {
	if r == nil || factory == nil {
		return errors.New("scheduler filter factory is nil")
	}
	kind, name = normalizeType(kind), normalizeName(name)
	if name == "" {
		return errors.New("scheduler filter name is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filters[kind] == nil {
		r.filters[kind] = make(map[string]FilterFactory)
	}
	if _, exists := r.filters[kind][name]; exists {
		return fmt.Errorf("%w: filter %s/%s", ErrDuplicateRegistration, kind, name)
	}
	r.filters[kind][name] = factory
	return nil
}

func (r *Registry) RegisterScore(kind, name string, factory ScoreFactory) error {
	if r == nil || factory == nil {
		return errors.New("scheduler score factory is nil")
	}
	kind, name = normalizeType(kind), normalizeName(name)
	if name == "" {
		return errors.New("scheduler score name is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scores[kind] == nil {
		r.scores[kind] = make(map[string]ScoreFactory)
	}
	if _, exists := r.scores[kind][name]; exists {
		return fmt.Errorf("%w: score %s/%s", ErrDuplicateRegistration, kind, name)
	}
	r.scores[kind][name] = factory
	return nil
}

func (r *Registry) RegisterFilterProvider(kind string, factory FilterFactory) error {
	if r == nil || factory == nil {
		return errors.New("scheduler filter provider is nil")
	}
	kind = normalizeType(kind)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.filterProviders[kind]; exists {
		return fmt.Errorf("%w: filter provider %s", ErrDuplicateRegistration, kind)
	}
	r.filterProviders[kind] = factory
	return nil
}

func (r *Registry) RegisterScoreProvider(kind string, factory ScoreFactory) error {
	if r == nil || factory == nil {
		return errors.New("scheduler score provider is nil")
	}
	kind = normalizeType(kind)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.scoreProviders[kind]; exists {
		return fmt.Errorf("%w: score provider %s", ErrDuplicateRegistration, kind)
	}
	r.scoreProviders[kind] = factory
	return nil
}

func (r *Registry) BuildFilter(ctx context.Context, conf config.SchedulerProfilePluginConf) (selector filter.Selector, err error) {
	if r == nil {
		return nil, errors.New("scheduler plugin registry is nil")
	}
	kind, name := normalizeType(conf.Type), normalizeName(conf.Name)
	r.mu.RLock()
	factory := r.filters[kind][name]
	if factory == nil {
		factory = r.filterProviders[kind]
	}
	// A kind registered under either phase is a known type: only report
	// ErrUnknownPluginType when the kind is entirely absent, so a typo'd
	// filter name on a scores-only registry surfaces as "unknown plugin"
	// instead of the misleading "unknown type".
	_, knownType := r.filters[kind]
	if !knownType {
		_, knownType = r.scores[kind]
	}
	if _, ok := r.filterProviders[kind]; ok {
		knownType = true
	} else if _, ok := r.scoreProviders[kind]; ok {
		knownType = true
	}
	r.mu.RUnlock()
	if factory == nil {
		if !knownType {
			return nil, fmt.Errorf("%w: filter type %q", ErrUnknownPluginType, kind)
		}
		return nil, fmt.Errorf("%w: filter %s/%s", ErrUnknownPlugin, kind, name)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			selector = nil
			err = fmt.Errorf("build filter %s/%s: %v", kind, name, recovered)
		}
	}()
	selector, err = factory(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("build filter %s/%s: %w", kind, name, err)
	}
	if selector == nil {
		return nil, fmt.Errorf("build filter %s/%s: factory returned nil", kind, name)
	}
	return selector, nil
}

func (r *Registry) BuildScore(ctx context.Context, conf config.SchedulerProfilePluginConf) (selector score.Selector, err error) {
	if r == nil {
		return nil, errors.New("scheduler plugin registry is nil")
	}
	kind, name := normalizeType(conf.Type), normalizeName(conf.Name)
	r.mu.RLock()
	factory := r.scores[kind][name]
	if factory == nil {
		factory = r.scoreProviders[kind]
	}
	// See BuildFilter: a kind registered under either phase is a known type.
	_, knownType := r.scores[kind]
	if !knownType {
		_, knownType = r.filters[kind]
	}
	if _, ok := r.scoreProviders[kind]; ok {
		knownType = true
	} else if _, ok := r.filterProviders[kind]; ok {
		knownType = true
	}
	r.mu.RUnlock()
	if factory == nil {
		if !knownType {
			return nil, fmt.Errorf("%w: score type %q", ErrUnknownPluginType, kind)
		}
		return nil, fmt.Errorf("%w: score %s/%s", ErrUnknownPlugin, kind, name)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			selector = nil
			err = fmt.Errorf("build score %s/%s: %v", kind, name, recovered)
		}
	}()
	selector, err = factory(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("build score %s/%s: %w", kind, name, err)
	}
	if selector == nil {
		return nil, fmt.Errorf("build score %s/%s: factory returned nil", kind, name)
	}
	return selector, nil
}

// Names returns stable, human-readable registered names for diagnostics.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for kind, entries := range r.filters {
		for name := range entries {
			names = append(names, "filter/"+kind+"/"+name)
		}
	}
	for kind, entries := range r.scores {
		for name := range entries {
			names = append(names, "score/"+kind+"/"+name)
		}
	}
	for kind := range r.filterProviders {
		names = append(names, "filter/"+kind+"/*")
	}
	for kind := range r.scoreProviders {
		names = append(names, "score/"+kind+"/*")
	}
	sort.Strings(names)
	return names
}
