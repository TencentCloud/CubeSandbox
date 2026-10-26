// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"testing"
)

func TestRecover(t *testing.T) {
	defer Recover()
	panic("a panic")
}

func TestRecoverNil(t *testing.T) {
	defer Recover()
	panic(nil)
}
