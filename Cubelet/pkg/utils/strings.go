// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
	Package util common tools

* Copyright (c) 2020 Tencent Serverless
* All rights reserved
* Author: jiangdu
* Date: 2020-06-15
*/
package utils

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"unsafe"

	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/util/sets"
)

type stdJSONTool struct{}

func (stdJSONTool) Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (stdJSONTool) Unmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

var JSONTool = stdJSONTool{}

func String2Slice(s string) []byte {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh := reflect.SliceHeader{
		Data: sh.Data,
		Len:  sh.Len,
		Cap:  sh.Len,
	}
	return *(*[]byte)(unsafe.Pointer(&bh))
}

func InStringSlice(ss []string, str string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, str) {
			return true
		}
	}
	return false
}

func Decode(body string, req interface{}) error {
	return json.Unmarshal([]byte(body), req)
}

func HashCode(dimension string) uint32 {
	return crc32.ChecksumIEEE(String2Slice(dimension))
}

func InterfaceToString(obj interface{}) string {
	body, _ := json.Marshal(obj)
	return string(body)
}

func IsDebug() bool {
	return cubelog.GetLevel() == cubelog.DEBUG
}

var ErrLimitReached = errors.New("the read limit is reached")

func ReadAtMost(r io.Reader, limit int64) ([]byte, error) {
	limitedReader := &io.LimitedReader{R: r, N: limit}
	data, err := ioutil.ReadAll(limitedReader)
	if err != nil {
		return data, err
	}
	if limitedReader.N <= 0 {
		return data, ErrLimitReached
	}
	return data, nil
}

func MergeStringSlices(a []string, b []string) []string {
	set := sets.NewString(a...)
	set.Insert(b...)
	return set.UnsortedList()
}

func MaxCommonPrefix(arrays []string) string {
	if len(arrays) == 0 {
		return ""
	}

	prefix := arrays[0]

	for i := 1; i < len(arrays); i++ {

		j := 0
		for j < len(prefix) && j < len(arrays[i]) && prefix[j] == arrays[i][j] {
			j++
		}
		prefix = prefix[:j]

		if len(prefix) == 0 {
			return ""
		}
	}

	return prefix
}

func RemoveStringPrefix(arrays []string, prefix string) []string {
	for i := range arrays {
		arrays[i] = strings.TrimPrefix(arrays[i], prefix)
	}
	return arrays
}
