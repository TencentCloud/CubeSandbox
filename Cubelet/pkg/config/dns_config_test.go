// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreHandleNormalizesDefaultDNSServers(t *testing.T) {
	cfg, err := preHandle(&Config{
		Common: &CommonConf{
			DefaultDNSServers:  []string{" 119.29.29.29 ", "", "1.1.1.1"},
			DefaultDNSSearches: []string{" default.svc.cluster.local ", "", "svc.cluster.local"},
			DefaultDNSOptions:  []string{" ndots:5 ", ""},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "119.29.29.29,1.1.1.1", strings.Join(cfg.Common.DefaultDNSServers, ","))
	assert.Equal(t, "default.svc.cluster.local,svc.cluster.local", strings.Join(cfg.Common.DefaultDNSSearches, ","))
	assert.Equal(t, "ndots:5", strings.Join(cfg.Common.DefaultDNSOptions, ","))
}

func TestValidateRejectsInvalidDefaultDNSServers(t *testing.T) {
	err := validate(&Config{
		Common: &CommonConf{
			DefaultDNSServers: []string{"invalid-ip"},
		},
		HostConf: defaultHostConf(),
	})
	require.Error(t, err)
}

func TestValidateRejectsInvalidDefaultDNSSearchesAndOptions(t *testing.T) {
	err := validate(&Config{
		Common: &CommonConf{
			DefaultDNSSearches: []string{"bad domain"},
		},
		HostConf: defaultHostConf(),
	})
	require.Error(t, err)

	err = validate(&Config{
		Common: &CommonConf{
			DefaultDNSOptions: []string{"ndots:5\nnameserver 1.1.1.1"},
		},
		HostConf: defaultHostConf(),
	})
	require.Error(t, err)
}

func TestValidateTemplateSettle(t *testing.T) {
	// Zero value (section absent) and in-range values pass.
	require.NoError(t, validate(&Config{Common: &CommonConf{}, HostConf: defaultHostConf()}))
	require.NoError(t, validate(&Config{
		Common: &CommonConf{TemplateSettle: TemplateSettleConf{
			BusyThreshold: 0.1,
			QuietRatio:    0.8,
			MaxWait:       60 * time.Second,
		}},
		HostConf: defaultHostConf(),
	}))

	negative := -time.Second
	for name, ts := range map[string]TemplateSettleConf{
		"busy_threshold >= 1":  {BusyThreshold: 1},
		"negative quiet_ratio": {QuietRatio: -0.1},
		"quiet_ratio > 1":      {QuietRatio: 1.5},
		"negative max_wait":    {MaxWait: -time.Second},
		"negative bail_after":  {BailAfter: &negative},
	} {
		err := validate(&Config{
			Common:   &CommonConf{TemplateSettle: ts},
			HostConf: defaultHostConf(),
		})
		require.Error(t, err, name)
	}
}
