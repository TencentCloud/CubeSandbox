// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"io"
	"time"
)

var (
	enableLogMetric = false

	traceStd *Logger
)

type RequestTrace struct {
	DestID         int64
	Region         string
	AppID          int64
	RequestID      string
	Action         string
	Qualifier      string
	InstanceID     string
	FunctionName   string
	Namespace      string
	VersionID      string
	Timestamp      time.Time
	Caller         string
	Callee         string
	CallerIP       string
	CalleeEndpoint string
	CalleeAction   string
	ErrorCode      ErrorCode
	SubErrorCode   string
	Cost           time.Duration
	RetCode        int64
	Version        string
	Cluster        string
	ContainerID    string
	ColdStart      float64
	Duration       int64
	ErrorSource    string
	CvmId          string
	Runtime        string
	CalleeCluster  string
	FunctionType   string
	DeployMode     string
	InstanceType   string
}

// DeepCopy returns an independent copy of the trace. A nil receiver is
// intentionally tolerated: non-request paths (background workers, tests) have
// no trace in context, and callers in those paths may invoke DeepCopy on the
// nil result of GetTraceInfo. In that case we return a fresh, empty trace
// rather than panicking, so this nil guard is deliberate and must not be
// removed as dead code.
func (rt *RequestTrace) DeepCopy() *RequestTrace {
	if rt == nil {
		return new(RequestTrace)
	}
	o := new(RequestTrace)
	*o = *rt
	return o
}

func (rt *RequestTrace) WithCallee(callee string) *RequestTrace {
	rt.Callee = callee
	return rt
}

func init() {

	traceStd = GetLogger("Trace")
	traceStd.SetOutput(nil)

}

func Trace(trace *RequestTrace) {
	cost := float64(trace.Cost.Nanoseconds()/1000) / 1000

	region := trace.Region
	if region == "" {
		region = string(defaultRegion)
	}
	tmcluster := trace.Cluster
	if tmcluster == "" {
		tmcluster = cluster
	}
	version := trace.Version
	if version == "" {
		version = moduleVersion
	}

	if enableLogMetric {
		fields := makeLogFieldsFromTrace(trace)
		fields["CostTime"] = cost

		if traceStd.writer != nil {
			traceStd.WithFields(fields).Errorf("")
		} else {
			std.WithFields(fields).Errorf("")
		}
	}
}

func EnableLogMetric() {
	enableLogMetric = true
}

func DisableLogMetric() {
	enableLogMetric = false
}

func SetTraceOutput(w io.Writer) {
	traceStd.writer = w
}
