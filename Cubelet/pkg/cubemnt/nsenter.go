// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux && !gccgo
// +build linux,!gccgo

package cubemnt

/*
#cgo CFLAGS: -Wall
#include <stdlib.h>
extern void enter_namespace();
extern int enter_namespace_path(const char *path);
void __attribute__((constructor)) init(void) {
	enter_namespace();
}
*/
import "C"
import "unsafe"

func enterNamespacePath(path string) int {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	return int(C.enter_namespace_path(cpath))
}
