// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestDistributionWorkerCount(t *testing.T) {
	original := distributionConfig
	t.Cleanup(func() { distributionConfig = original })

	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{name: "no config", want: config.DefaultDistributionWorkers},
		{name: "no common config", cfg: &config.Config{}, want: config.DefaultDistributionWorkers},
		{name: "unset", cfg: &config.Config{Common: &config.CommonConf{}}, want: config.DefaultDistributionWorkers},
		{name: "negative", cfg: &config.Config{Common: &config.CommonConf{DistributionWorkers: -1}}, want: config.DefaultDistributionWorkers},
		{name: "configured", cfg: &config.Config{Common: &config.CommonConf{DistributionWorkers: 11}}, want: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			distributionConfig = func() *config.Config { return tc.cfg }
			if got := distributionWorkerCount(); got != tc.want {
				t.Fatalf("distributionWorkerCount() = %d, want %d", got, tc.want)
			}
		})
	}
}
