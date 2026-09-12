// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package grpcplugin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestClassifyRPCError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"version mismatch", fmt.Errorf("wrap: %w", ErrVersionMismatch), rpcReasonVersionMismatch},
		{"circuit open", fmt.Errorf("wrap: %w", ErrCircuitOpen), rpcReasonCircuitOpen},
		{"context deadline", context.DeadlineExceeded, rpcReasonTimeout},
		{"grpc deadline", status.Error(codes.DeadlineExceeded, "deadline"), rpcReasonTimeout},
		{"unavailable", status.Error(codes.Unavailable, "down"), rpcReasonError},
		{"plain error", errors.New("boom"), rpcReasonError},
	}
	for _, tc := range cases {
		if got := classifyRPCError(tc.err); got != tc.want {
			t.Errorf("%s: classifyRPCError = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func histogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	metric, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", labels, err)
	}
	histogram, ok := metric.(prometheus.Histogram)
	if !ok {
		t.Fatalf("metric %v is not a histogram", labels)
	}
	m := &dto.Metric{}
	if err := histogram.Write(m); err != nil {
		t.Fatalf("histogram Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestExternalPluginRPCMetrics(t *testing.T) {
	connection, server := startPluginServer(t)
	client, err := newClientFromConn(context.Background(), config.SchedulerProfilePluginConf{
		Name: "fake", Timeout: time.Second,
	}, "filter", connection)
	if err != nil {
		t.Fatal(err)
	}
	selector := &filterPlugin{client: client}
	t.Cleanup(func() { _ = selector.Close() })

	durationBefore := histogramSampleCount(t, grpcRPCDuration, "fake", rpcMethodFilter)
	syncDurationBefore := histogramSampleCount(t, grpcRPCDuration, "fake", rpcMethodSyncSnapshot)
	requestBytesBefore := testutil.ToFloat64(grpcRPCBytes.WithLabelValues("fake", rpcMethodFilter, rpcDirectionRequest))
	responseBytesBefore := testutil.ToFloat64(grpcRPCBytes.WithLabelValues("fake", rpcMethodFilter, rpcDirectionResponse))
	errorsBefore := testutil.ToFloat64(grpcRPCErrors.WithLabelValues("fake", rpcMethodFilter, rpcReasonError))

	if _, err := selector.Select(grpcSelection()); err != nil {
		t.Fatal(err)
	}
	if got := histogramSampleCount(t, grpcRPCDuration, "fake", rpcMethodFilter); got != durationBefore+1 {
		t.Fatalf("filter rpc duration count delta = %v, want 1", got-durationBefore)
	}
	if got := histogramSampleCount(t, grpcRPCDuration, "fake", rpcMethodSyncSnapshot); got != syncDurationBefore+1 {
		t.Fatalf("sync snapshot rpc duration count delta = %v, want 1", got-syncDurationBefore)
	}
	if got := testutil.ToFloat64(grpcRPCBytes.WithLabelValues("fake", rpcMethodFilter, rpcDirectionRequest)); got <= requestBytesBefore {
		t.Fatalf("filter request bytes delta = %v, want > 0", got-requestBytesBefore)
	}
	if got := testutil.ToFloat64(grpcRPCBytes.WithLabelValues("fake", rpcMethodFilter, rpcDirectionResponse)); got <= responseBytesBefore {
		t.Fatalf("filter response bytes delta = %v, want > 0", got-responseBytesBefore)
	}

	// A failing RPC lands in the classified error bucket; the circuit breaker
	// counts it, and the snapshot is already synced so only Filter is called.
	server.mu.Lock()
	server.failFilter = true
	server.mu.Unlock()
	if _, err := selector.Select(grpcSelection()); err == nil {
		t.Fatal("failing plugin must return an error")
	}
	if got := testutil.ToFloat64(grpcRPCErrors.WithLabelValues("fake", rpcMethodFilter, rpcReasonError)); got != errorsBefore+1 {
		t.Fatalf("filter rpc error delta = %v, want 1", got-errorsBefore)
	}
}
