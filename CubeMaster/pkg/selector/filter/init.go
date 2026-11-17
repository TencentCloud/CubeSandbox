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

// Selector is the filter plugin interface: Select keeps the nodes that satisfy
// the plugin's condition, and ID identifies the plugin.
type Selector interface {
	Select(selCtx *selctx.SelectorCtx) (node.NodeList, error)

	ID() string
}

// NewSelector builds the enabled filter plugins from the configured filter
// name list via reflection.
func NewSelector() []Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.Filter == nil {
		return []Selector{}
	}
	ss := make([]Selector, 0)
	for _, name := range conf.Filter.EnableFilters {

		fn := reflect.ValueOf(filters[name])

		if !fn.IsValid() {
			continue
		}
		ss = append(ss, fn.Call(nil)[0].Interface().(Selector))
	}
	return ss
}

// filters maps filter config names to constructor functions.
var filters = map[string]interface{}{
	"cpu":                 NewCpuFilter,
	"mem":                 NewMemFilter,
	"template_locality":   NewTemplateLocalityFilter,
	"realtime_create_num": NewRealtimecreatelimit,
	"disk":                NewDiskFilter,
	"thirtparty":          NewThirtpartyFilter,
}
