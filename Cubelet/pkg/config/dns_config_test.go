// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"strings"
	"testing"

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
