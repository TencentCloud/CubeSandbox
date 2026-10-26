// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package utils 工具类
package utils

import (
	"io"
	"reflect"
	"unsafe"

	"slices"

	jsoniter "github.com/json-iterator/go"
)

var JSONTool = jsoniter.ConfigFastest

func InterfaceToString(obj interface{}) string {
	body, _ := JSONTool.Marshal(obj)
	return string(body)
}

func DecodeHttpBody(body io.ReadCloser, obj interface{}) error {
	decoder := jsoniter.NewDecoder(body)
	decoder.UseNumber()
	return decoder.Decode(obj)
}

func String2Slice(s string) []byte {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh := reflect.SliceHeader{
		Data: sh.Data,
		Len:  sh.Len,
		Cap:  sh.Len,
	}
	return *(*[]byte)(unsafe.Pointer(&bh))
}

func Int64Ptr(v int64) *int64 {
	return &v
}

func StringPtr(v string) *string {
	return &v
}

func StringPtrs(vals []string) []*string {
	ptrs := make([]*string, len(vals))
	for i := 0; i < len(vals); i++ {
		ptrs[i] = &vals[i]
	}
	return ptrs
}

func InSlice(s string, slice ...string) bool {
	return Contains(s, slice)
}

func Contains(val string, slice []string) bool {
	return slices.Contains(slice, val)
}

func SliceToMap(slice []string) map[string]any {
	set := make(map[string]any, len(slice))
	for _, s := range slice {
		set[s] = struct{}{}
	}
	return set
}

func FirstKey(m map[string]any) string {
	for k := range m {
		return k
	}
	return ""
}

func SumSliceInt64(s []int64) int64 {
	sum := int64(0)
	for _, v := range s {
		sum += v
	}
	return sum
}

func MapToSlice[K comparable, V any](m map[K]V) []K {
	if m == nil {
		return nil
	}

	result := make([]K, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}
