// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux && gccgo
// +build linux,gccgo

package cubemnt

/*
#cgo CFLAGS: -Wall
extern void enter_namespace();
void __attribute__((constructor)) init(void) {
	enter_namespace();
}
*/
import "C"

var AlwaysFalse bool

func init() {
	if AlwaysFalse {

		C.init()
	}
}
