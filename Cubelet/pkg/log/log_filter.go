// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"fmt"
	"reflect"
	"strings"

	jsoniter "github.com/json-iterator/go"
)

const (
	LogFiledPrefix = "LogIndex"

	MaxFieldValueLength = 100
)

type fieldAction int

const (
	fieldActionKeep fieldAction = iota

	fieldActionExtract

	fieldActionDelete
)

var (
	wellknownFields = []string{
		"RequestId",
		"Action",
		"Caller",
		"Callee",
		"CalleeEndpoint",
		"CalleeAction",
		"InstanceId",
		"RetCode",
		"RetCode",
		"InstanceType",
		"AppId",
		"Namespace",
		"FunctionType",
		"Cluster",
		"CalleeCluster",
		"LocalIp",
		"Module",
		"CodeLine",
		"@timestamp",
		"LogLevel",
		"LogContent",

		"request_id",
		"action",
		"caller",
		"caller_ip",
		"callee_endpoint",
		"callee_action",
		"cost_time",
		"instance_id",
		"app_id",
		"container_id",
		"callee_cluster",
		"function_type",
		"deploy_mode",
		"instance_type",

		"level",
		"source",
		"method",
	}
	wellknownFieldsMap = map[string]struct{}{}
)

func init() {
	for _, field := range wellknownFields {
		wellknownFieldsMap[strings.ToLower(field)] = struct{}{}
	}
}

func (w *CubeWrapperLogEntry) extralFieldsToContent() string {
	data := w.Entry.GetFields()
	extendsFields := make(map[string]any, len(data))

	for k, v := range data {

		if v == nil {
			delete(data, k)
			continue
		}

		lowerKey := strings.ToLower(k)
		if _, ok := wellknownFieldsMap[lowerKey]; ok {
			continue
		}

		action := classifyFieldAction(v)

		switch action {
		case fieldActionKeep:

			continue
		case fieldActionExtract:

			extendsFields[k] = v
			delete(data, k)
		case fieldActionDelete:

			delete(data, k)
		}
	}

	if len(extendsFields) > 0 {
		jsonStr, _ := jsoniter.MarshalToString(extendsFields)
		return fmt.Sprintf(" with additional fields: %s", jsonStr)
	}
	return ""
}

func classifyFieldAction(v any) fieldAction {
	if v == nil {
		return fieldActionDelete
	}

	rv := reflect.ValueOf(v)
	rt := rv.Type()

	switch rt.Kind() {
	case reflect.Func:

		return fieldActionDelete
	case reflect.Chan:

		return fieldActionDelete
	case reflect.UnsafePointer:

		return fieldActionDelete
	}

	switch val := v.(type) {
	case string:

		if len(val) < MaxFieldValueLength {
			return fieldActionKeep
		}
		return fieldActionExtract
	case int, int8, int16, int32, int64:

		return fieldActionKeep
	case uint, uint8, uint16, uint32, uint64:

		return fieldActionKeep
	case float32, float64:

		return fieldActionKeep
	case bool:

		return fieldActionKeep
	}

	if rt.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return fieldActionDelete
		}

		elem := rt.Elem()
		switch elem.Kind() {
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:

			return fieldActionDelete
		}

		return fieldActionExtract
	}

	return fieldActionExtract
}
