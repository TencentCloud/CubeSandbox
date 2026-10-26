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

var filters = map[string]interface{}{
	"cpu":                 NewCpuFilter,
	"mem":                 NewMemFilter,
	"template_locality":   NewTemplateLocalityFilter,
	"realtime_create_num": NewRealtimecreatelimit,
	"disk":                NewDiskFilter,
	"thirtparty":          NewThirtpartyFilter,
}
