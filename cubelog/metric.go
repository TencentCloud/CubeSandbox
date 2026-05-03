// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"bytes"
	"io"
	"reflect"
	"strconv"
	"time"
	"unsafe"
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

func (rt *RequestTrace) DeepCopy() *RequestTrace {
	o := new(RequestTrace)
	*o = *rt
	return o
}

func (rt *RequestTrace) WithCallee(callee string) *RequestTrace {
	rt.Callee = callee
	return rt
}

type requestTrace struct {
	Timestamp      int64
	AppId          int64
	FunctionId     string
	Action         string
	Qualifier      string
	Region         string
	Cluster        string
	Version        string
	Caller         string
	Callee         string
	CallerIP       string
	CalleeEndpoint string
	CalleeAction   string
	ErrorCode      int64
	CalleeCluster  string
	FunctionType   string
	DeployMode     string

	ReqCnt       uint64
	SuccCnt      uint64
	FailCnt      uint64
	ColdStartCnt uint64

	ColdStart          float64
	ColdStartSum       float64
	ColdStartMax       float64
	ColdStartMin       float64
	ColdStartRange1Cnt uint64
	ColdStartRange2Cnt uint64
	ColdStartRange3Cnt uint64
	ColdStartRange4Cnt uint64
	ColdStartRange5Cnt uint64
	ColdStartRange6Cnt uint64
	ColdStartRange7Cnt uint64
	ColdStartRange8Cnt uint64

	Cost          float64
	CostSum       float64
	CostMax       float64
	CostMin       float64
	CostRange1Cnt uint64
	CostRange2Cnt uint64
	CostRange3Cnt uint64
	CostRange4Cnt uint64
	CostRange5Cnt uint64
	CostRange6Cnt uint64
	CostRange7Cnt uint64
	CostRange8Cnt uint64
}

func (m *requestTrace) indexKey() []byte {
	size := 1 +
		len(m.Region) + len(m.Cluster) + len(m.Callee) +
		len(m.CalleeAction) + len(m.CalleeEndpoint)
	b := make([]byte, 0, size)
	buf := bytes.NewBuffer(b)
	buf.WriteString(m.Region)
	buf.WriteString(m.Cluster)
	buf.WriteString(m.Callee)
	buf.WriteString(m.CalleeAction)
	buf.WriteString(m.CalleeEndpoint)
	return buf.Bytes()
}

func (m *requestTrace) realKey() string {
	errorCode := strconv.FormatInt(m.ErrorCode, 10)
	appId := strconv.FormatInt(m.AppId, 10)
	size := 1 +
		len(m.Region) + len(m.Cluster) + len(m.CalleeCluster) + len(m.FunctionType) + len(m.DeployMode) +
		len(m.Version) + len(m.Caller) + len(m.CallerIP) +
		len(m.Callee) + len(m.CalleeAction) + len(m.CalleeEndpoint) +
		len(errorCode) + len(appId) + len(m.FunctionId) +
		len(m.Action) + len(m.Qualifier)
	b := make([]byte, 0, size)
	buf := bytes.NewBuffer(b)

	buf.WriteString(m.Region)
	buf.WriteString(m.Cluster)
	buf.WriteString(m.CalleeCluster)
	buf.WriteString(m.FunctionType)
	buf.WriteString(m.DeployMode)
	buf.WriteString(m.Version)
	buf.WriteString(m.Caller)
	buf.WriteString(m.CallerIP)
	buf.WriteString(m.Callee)
	buf.WriteString(m.CalleeAction)
	buf.WriteString(m.CalleeEndpoint)
	buf.WriteString(errorCode)
	buf.WriteString(appId)
	buf.WriteString(m.FunctionId)
	buf.WriteString(m.Action)
	buf.WriteString(m.Qualifier)
	return slice2Str(buf.Bytes())
}

func slice2Str(b []byte) string {
	bh := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	sh := reflect.StringHeader{
		Data: bh.Data,
		Len:  bh.Len,
	}
	return *(*string)(unsafe.Pointer(&sh))
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
