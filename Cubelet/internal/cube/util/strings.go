// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package util

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

func InStringSlice(ss []string, str string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, str) {
			return true
		}
	}
	return false
}

func SubtractStringSlice(ss []string, str string) []string {
	var res []string
	for _, s := range ss {
		if strings.EqualFold(s, str) {
			continue
		}
		res = append(res, s)
	}
	return res
}

func MergeStringSlices(a []string, b []string) []string {
	set := sets.NewString(a...)
	set.Insert(b...)
	return set.UnsortedList()
}
