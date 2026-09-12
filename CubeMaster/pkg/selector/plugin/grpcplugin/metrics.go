// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package grpcplugin

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// RPC label value enums. Method and reason sets are closed; the plugin name
// comes from the operator's config, a bounded set, so it is safe as a label.
const (
	rpcMethodHandshake    = "handshake"
	rpcMethodSyncSnapshot = "sync_snapshot"
	rpcMethodFilter       = "filter"
	rpcMethodScore        = "score"

	rpcDirectionRequest  = "request"
	rpcDirectionResponse = "response"

	rpcReasonTimeout         = "timeout"
	rpcReasonVersionMismatch = "version_mismatch"
	rpcReasonCircuitOpen     = "circuit_open"
	rpcReasonError           = "error"
)

var (
	grpcRPCDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "scheduler_grpc_plugin_rpc_duration_seconds",
		Help:    "Latency of a single external scheduler plugin RPC (including circuit-open fast failures), by plugin and method.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14), // 100µs .. ~0.8s
	}, []string{"plugin", "method"})

	grpcRPCErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scheduler_grpc_plugin_rpc_errors_total",
		Help: "Total external scheduler plugin RPC failures, by plugin, method and classified reason (timeout/error/version_mismatch/circuit_open).",
	}, []string{"plugin", "method", "reason"})

	grpcRPCBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scheduler_grpc_plugin_rpc_bytes_total",
		Help: "Total proto-serialized bytes exchanged with external scheduler plugins, by plugin, method and direction (request/response).",
	}, []string{"plugin", "method", "direction"})
)

// classifyRPCError maps an RPC failure to a low-cardinality reason enum. The
// context deadline is checked both as a plain error and as a gRPC status,
// since transport timeouts surface as codes.DeadlineExceeded.
func classifyRPCError(err error) string {
	switch {
	case errors.Is(err, ErrVersionMismatch):
		return rpcReasonVersionMismatch
	case errors.Is(err, ErrCircuitOpen):
		return rpcReasonCircuitOpen
	case errors.Is(err, context.DeadlineExceeded), status.Code(err) == codes.DeadlineExceeded:
		return rpcReasonTimeout
	default:
		return rpcReasonError
	}
}

// observeRPC records duration, byte volume and (on failure) the classified
// error of one plugin RPC. Duration and bytes are recorded for successes and
// failures alike; response bytes are skipped when the call returned no
// response.
func (c *client) observeRPC(method string, start time.Time, request, response proto.Message, err error) {
	grpcRPCDuration.WithLabelValues(c.name, method).Observe(time.Since(start).Seconds())
	if request != nil {
		grpcRPCBytes.WithLabelValues(c.name, method, rpcDirectionRequest).Add(float64(proto.Size(request)))
	}
	if response != nil {
		grpcRPCBytes.WithLabelValues(c.name, method, rpcDirectionResponse).Add(float64(proto.Size(response)))
	}
	if err != nil {
		grpcRPCErrors.WithLabelValues(c.name, method, classifyRPCError(err)).Inc()
	}
}
