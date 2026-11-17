// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"testing"
	"time"
)

func BenchmarkPluginDurationLookup(b *testing.B) {
	b.Run("label", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ObservePluginDuration("mixed_binpack", pluginKindGuard, "cpu", time.Microsecond)
		}
	})
	resolved := pluginDuration.WithLabelValues("mixed_binpack", "cpu", pluginKindGuard)
	b.Run("cached", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			resolved.Observe(time.Microsecond.Seconds())
		}
	})
}
