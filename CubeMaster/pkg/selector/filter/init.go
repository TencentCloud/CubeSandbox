// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package filter provides filter functions for node.Node.
package filter

import (
	"reflect"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type Selector interface {
	Select(selCtx *selctx.SelectorCtx) (node.NodeList, error)

	ID() string
}

func NewSelector() []Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil {
		return []Selector{}
	}
	return buildSelectors(conf)
}

func buildSelectors(conf *config.WrapperSchedulerConf) []Selector {
	ss := make([]Selector, 0)
	enableFilters := []string(nil)
	if conf.Filter != nil {
		enableFilters = conf.Filter.EnableFilters
	}
	seen := make(map[string]struct{}, len(enableFilters)+1)
	for _, name := range enableFilters {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		fn := reflect.ValueOf(filters[name])
		if !fn.IsValid() {
			continue
		}
		ss = append(ss, fn.Call(nil)[0].Interface().(Selector))
	}
	if conf.ScarceResourceFilterEnabled() {
		if _, ok := seen["scarce_resource"]; !ok {
			ss = append(ss, NewScarceResourceFilter())
		}
	}
	return ss
}

var filters = map[string]interface{}{
	"cpu":                 NewCpuFilter,
	"mem":                 NewMemFilter,
	"template_locality":   NewTemplateLocalityFilter,
	"realtime_create_num": NewRealtimecreatelimit,
	"disk":                NewDiskFilter,
	"thirtparty":          NewThirtpartyFilter,
	"scarce_resource":     NewScarceResourceFilter,
}
