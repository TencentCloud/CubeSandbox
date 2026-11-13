// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// Component evidence: synthetic tasks intentionally isolate storage capacity and
// the actual 30-second Writer window. Business-entrypoint evidence is separate.
func TestP6CapacityWriterWindowAndRecovery(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "capacity"
		if fault {
			name = "requests-dropped-before-redis"
		}
		t.Run(name, func(t *testing.T) {
			o := testOptions()
			o.MaxRecords = 1
			s, conf := newTestStore(t, o)
			var outage atomic.Bool
			var store DeliveryStore = s
			if fault {
				addr, mode := faultProxy(t, conf)
				conf.Nodes = addr
				proxy, err := NewRedisDeliveryStore(conf, o)
				require.NoError(t, err)
				defer proxy.Close()
				proxy.keys = s.keys
				outage.Store(true)
				store = &faultStore{DeliveryStore: s, put: func(ctx context.Context, task DeliveryTask) error {
					if outage.Load() {
						mode.Store(2)
					} // close before forwarding: Redis did not execute
					return proxy.Put(ctx, task)
				}}
			}
			held := testTask()
			held.NextAttemptAt = time.Now().Add(time.Hour)
			require.NoError(t, s.Put(testCtx, held))
			before := record(t, s, taskKey(held))
			c := localConf()
			c.IngressCapacity = 2
			c.Subscriptions = []Subscription{{"capacity-p6", true, "https://example.test/unused", []string{TypeCreated}}}
			outcome := "abandoned"
			if fault {
				outcome = "unconfirmed"
			}
			counter := func() float64 {
				m := &dto.Metric{}
				require.NoError(t, deliveryCounts.WithLabelValues("persist", outcome).Write(m))
				return m.GetCounter().GetValue()
			}
			initial := counter()
			r := runtimeFor(t, c, store)
			cap := runtimeCaptured(t)
			require.Equal(t, PublishAccepted, r.TryPublish(cap).Status)
			// The stored record is not due, so workers cannot make space. Wait for the
			// Writer to dequeue the primer before measuring saturated ingress.
			require.Eventually(t, func() bool { return len(r.buffer.Events()) == 0 }, time.Second, time.Millisecond)
			samples := make([]int64, 1000)
			full := 0
			start := time.Now()
			for i := range samples {
				tick := time.Now()
				status := r.TryPublish(cap).Status
				samples[i] = time.Since(tick).Nanoseconds()
				if status == PublishFull {
					full++
				}
			}
			require.Positive(t, full)
			require.LessOrEqual(t, len(r.buffer.Events()), 2)
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			t.Logf("component publish n=1000 full=%d p50=%dns p95=%dns p99=%dns goroutines=%d heap=%d pool=%+v", full, samples[500], samples[950], samples[990], runtime.NumGoroutine(), memory.HeapAlloc, s.pool.Stats())
			require.Eventually(t, func() bool { return counter() > initial }, 35*time.Second, 20*time.Millisecond)
			require.GreaterOrEqual(t, time.Since(start), 29*time.Second)
			require.Equal(t, before, record(t, s, taskKey(held)), "capacity and maintenance must preserve unfinished record")
			// Make capacity available through ordinary terminal completion and retention,
			// never by deleting unfinished tasks. A queued Writer retry can now persist.
			outage.Store(false)
			claimed := claimOne(t, s, held.NextAttemptAt)
			require.NoError(t, s.Complete(testCtx, claimed.Receipt, TerminalResult{State: DeliverySucceeded}))
			n, err := s.Prune(testCtx, time.Now().Add(o.TerminalRetention+time.Second), 32)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Eventually(t, func() bool {
				return command(t, s, "HLEN", s.keys[0]) == int64(1)
			}, 8*time.Second, 20*time.Millisecond)
			t.Logf("writer abandonment observed after %s; capacity recovered", time.Since(start))

		})
	}
}
