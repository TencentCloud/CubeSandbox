// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestBuildSelectorsAutoRegistersScarceResource(t *testing.T) {
	conf := &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			Filter: &config.SchedulerFilterConf{
				EnableFilters: []string{"cpu"},
			},
			ScarceResourceFilter: &config.ScarceResourceFilterConf{Enable: true},
		},
	}
	selectors := buildSelectors(conf)
	found := false
	for _, s := range selectors {
		if s.ID() == constants.SelectorFilterID+"/"+"scarce_resource" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected scarce_resource filter to be auto-registered")
	}
}

func TestBuildSelectorsSkipsDuplicateScarceResource(t *testing.T) {
	conf := &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			Filter: &config.SchedulerFilterConf{
				EnableFilters: []string{"cpu", "scarce_resource"},
			},
			ScarceResourceFilter: &config.ScarceResourceFilterConf{Enable: true},
		},
	}
	count := 0
	for _, s := range buildSelectors(conf) {
		if s.ID() == constants.SelectorFilterID+"/"+"scarce_resource" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one scarce_resource filter, got %d", count)
	}
}

func TestBuildSelectorsDisabledScarceResource(t *testing.T) {
	conf := &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			Filter: &config.SchedulerFilterConf{
				EnableFilters: []string{"cpu"},
			},
			ScarceResourceFilter: &config.ScarceResourceFilterConf{Enable: false},
		},
	}
	for _, s := range buildSelectors(conf) {
		if s.ID() == constants.SelectorFilterID+"/"+"scarce_resource" {
			t.Fatal("scarce_resource should not be registered when disabled")
		}
	}
}

func TestBuildSelectorsNilFilterRegistersScarceResource(t *testing.T) {
	conf := &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			ScarceResourceFilter: &config.ScarceResourceFilterConf{Enable: true},
		},
	}
	selectors := buildSelectors(conf)
	if len(selectors) != 1 {
		t.Fatalf("expected only scarce_resource filter, got %d selectors", len(selectors))
	}
	if selectors[0].ID() != constants.SelectorFilterID+"/"+"scarce_resource" {
		t.Fatalf("unexpected selector id: %s", selectors[0].ID())
	}
}
