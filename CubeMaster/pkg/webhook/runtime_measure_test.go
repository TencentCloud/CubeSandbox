// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This measurement reports observations, not a benchmark threshold.
// Timing covers only the named caller operations. Allocation/heap samples are
// process-wide and include concurrent background work; benchmark below isolates
// caller allocations. IDs and sample storage are prepared outside measurement.
func TestPublishLatencyDistribution(t *testing.T) {
	for _, scenario := range []string{"nop", "enabled", "slow-http", "full-ingress", "capacity"} {
		for _, withCapture := range []bool{false, true} {
			for _, parallel := range []int{1, 4} {
				t.Run(scenario+map[bool]string{false: "/publish", true: "/capture-publish"}[withCapture]+map[int]string{1: "/c1", 4: "/c4"}[parallel], func(t *testing.T) {
					const n = 1000
					c := localConf()
					c.IngressCapacity = 1024
					c.Workers = 2
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if scenario == "slow-http" {
							select {
							case <-r.Context().Done():
							case <-time.After(300 * time.Millisecond):
							}
						}
						w.WriteHeader(204)
					}))
					defer srv.Close()
					c.Subscriptions = []Subscription{{ID: "measure", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
					var pub Publisher = NopPublisher{}
					var active *Runtime
					if scenario == "full-ingress" {
						b, _ := NewBufferedPublisher(1)
						b.TryPublish(runtimeCaptured(t))
						pub = b
					} else if scenario != "nop" {
						capacity := 2000
						if scenario == "capacity" {
							capacity = 1
						}
						store := memoryStore(t, capacity)
						if scenario == "capacity" {
							task := testTask()
							task.NextAttemptAt = time.Now().Add(time.Hour)
							require.NoError(t, store.Put(testCtx, task))
						}
						active = runtimeFor(t, c, store)
						pub = active
					}
					inputs := make([]CaptureInput, n)
					events := make([]*CapturedEvent, n)
					samples := make([]int64, n)
					for i := range inputs {
						inputs[i] = CaptureInput{ID: uuid.New(), Type: TypeCreated, Origin: OriginLifecycle, SandboxID: strings.Repeat("s", 256), OccurredAt: time.Now()}
						events[i], _ = Capture(inputs[i])
					}
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					baseG := runtime.NumGoroutine()
					var peakHeap atomic.Uint64
					peakHeap.Store(before.HeapAlloc)
					var peakG atomic.Int64
					peakG.Store(int64(baseG))
					var accepted atomic.Int64
					stop := make(chan struct{})
					monitorDone := make(chan struct{})
					go func() {
						defer close(monitorDone)
						tick := time.NewTicker(time.Millisecond)
						defer tick.Stop()
						for {
							select {
							case <-stop:
								return
							case <-tick.C:
								var m runtime.MemStats
								runtime.ReadMemStats(&m)
								if m.HeapAlloc > peakHeap.Load() {
									peakHeap.Store(m.HeapAlloc)
								}
								g := int64(runtime.NumGoroutine())
								if g > peakG.Load() {
									peakG.Store(g)
								}
							}
						}
					}()
					start := time.Now()
					var wg sync.WaitGroup
					for j := 0; j < parallel; j++ {
						wg.Add(1)
						go func(j int) {
							defer wg.Done()
							for i := j; i < n; i += parallel {
								begin := time.Now()
								ev := events[i]
								if withCapture {
									ev, _ = Capture(inputs[i])
								}
								v := pub.TryPublish(ev)
								samples[i] = time.Since(begin).Nanoseconds()
								if v.Status == PublishAccepted {
									accepted.Add(1)
								}
							}
						}(j)
					}
					wg.Wait()
					elapsed := time.Since(start)
					if active != nil {
						time.Sleep(600 * time.Millisecond)
					} // sample background resource activity, outside caller timing
					runtime.ReadMemStats(&after)
					close(stop)
					<-monitorDone
					if after.HeapAlloc > peakHeap.Load() {
						peakHeap.Store(after.HeapAlloc)
					}
					sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
					t.Logf("n=%d concurrency=%d sandbox_bytes=256 p50=%dns p95=%dns p99=%dns throughput=%.0f/s accepted=%d rejected=%d process_allocs/op=%.3f heap_peak_delta=%d goroutine_peak_delta=%d", n, parallel, samples[n/2], samples[n*95/100], samples[n*99/100], float64(n)/elapsed.Seconds(), accepted.Load(), n-accepted.Load(), float64(after.Mallocs-before.Mallocs)/n, int64(peakHeap.Load())-int64(before.HeapAlloc), peakG.Load()-int64(baseG))
					if active != nil {
						ctx, cancel := context.WithTimeout(testCtx, 300*time.Millisecond)
						active.Shutdown(ctx)
						cancel()
					}
				})
			}
		}
	}
}
func BenchmarkPublishP3(b *testing.B) {
	input := CaptureInput{ID: uuid.New(), Type: TypeCreated, Origin: OriginLifecycle, SandboxID: strings.Repeat("s", 256), OccurredAt: time.Now()}
	ev, _ := Capture(input)
	for _, capture := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			name := map[bool]string{false: "Publish", true: "CapturePublish"}[capture] + map[bool]string{false: "Nop", true: "Full"}[enabled]
			b.Run(name, func(b *testing.B) {
				var pub Publisher = NopPublisher{}
				if enabled {
					p, _ := NewBufferedPublisher(1)
					p.TryPublish(ev)
					pub = p
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v := ev
					if capture {
						v, _ = Capture(input)
					}
					pub.TryPublish(v)
				}
			})
		}
	}
}
