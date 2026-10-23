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

// Selector 过滤插件接口：Select 从候选节点中过滤出满足条件的节点，ID 标识插件
type Selector interface {
	Select(selCtx *selctx.SelectorCtx) (node.NodeList, error)

	ID() string
}

// NewSelector 根据配置中启用的过滤器名列表，通过反射构造并返回对应的过滤插件实例
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

// filters 注册表：过滤器配置名 -> 构造函数的映射
var filters = map[string]interface{}{
	"cpu":                 NewCpuFilter,
	"mem":                 NewMemFilter,
	"template_locality":   NewTemplateLocalityFilter,
	"realtime_create_num": NewRealtimecreatelimit,
	"disk":                NewDiskFilter,
	"thirtparty":          NewThirtpartyFilter,
}
