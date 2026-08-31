// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package tracecost

import (
	"container/list"
	"context"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"sync"
	"time"
)

type contextKey string

const KTraceCost contextKey = "kTraceCost"

func RecordStepInfo(ctx context.Context, step string) {
	timeCostTmp := ctx.Value(KTraceCost)
	if timeCostTmp != nil {
		timeCost, ok := timeCostTmp.(*TimeCost)
		if ok {
			timeCost.Add(step)
		}
	}
}

func ShowStepInfo(ctx context.Context) string {
	timeCostTmp := ctx.Value(KTraceCost)
	if timeCostTmp != nil {
		timeCost, ok := timeCostTmp.(*TimeCost)
		if ok {
			return timeCost.Show()
		}
	}
	return ""
}

type CostObj struct {
	Step       int
	Name       string
	InsertTime time.Time
}

type TimeCost struct {
	sync.Mutex
	Name     string
	Ctx      context.Context
	Index    int
	InitTime time.Time
	List     *list.List
}

func NewTimeCost(ctx context.Context, name string) *TimeCost {
	ctx = context.WithValue(ctx, CubeLog.KeyCalleeAction, "ShowStatBy"+name)
	return &TimeCost{
		Index:    1,
		Ctx:      ctx,
		Name:     name,
		InitTime: time.Now(),
		List:     list.New(),
	}
}

func (tc *TimeCost) Add(name string) {
	if tc == nil {
		return
	}
	tc.Lock()
	tc.List.PushBack(&CostObj{tc.Index, name, time.Now()})
	tc.Unlock()
	tc.Index++
}

func (tc *TimeCost) Show() string {
	if tc == nil {
		return ""
	}
	var ret string
	tc.Lock()
	defer tc.Unlock()
	for e := tc.List.Front(); e != nil; e = e.Next() {
		obj := e.Value.(*CostObj)
		prev := e.Prev()
		if prev != nil {
			ret += fmt.Sprintf("STEP: %d, Action: %s, Timestamp: %s, Use: %v, Since: %v\n",
				obj.Step, obj.Name, obj.InsertTime.Format("2006-01-02 15:04:05"), obj.InsertTime.Sub(prev.Value.(*CostObj).InsertTime), obj.InsertTime.Sub(tc.InitTime))
		} else {
			ret += fmt.Sprintf("STEP: %d, Action:%s, Timestamp: %s, Use: %v, Since: %v\n",
				obj.Step, obj.Name, obj.InsertTime.Format("2006-01-02 15:04:05"), obj.InsertTime.Sub(tc.InitTime), obj.InsertTime.Sub(tc.InitTime))
		}
	}
	return ret
}

func Close(tc *TimeCost) {
	tc = nil
}
