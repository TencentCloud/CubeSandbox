// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package util

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestSizeof(t *testing.T) {

	var a bool = true
	fmt.Println("a Bool size:", Sizeof(a))
	var b int = 1
	fmt.Println("b int size:", Sizeof(b))
	var c int8 = 2
	fmt.Println("c int8 size:", Sizeof(c))
	var d int16 = 3
	fmt.Println("d int16 size:", Sizeof(d))
	var e int32 = 4
	fmt.Println("e int32 size:", Sizeof(e))
	var f int64 = 5
	fmt.Println("f int64 size:", Sizeof(f))
	var g uint = 6
	fmt.Println("g uint size:", Sizeof(g))

	var h uint8 = 7
	fmt.Println("h uint8 size:", Sizeof(h))
	var i uint16 = 8
	fmt.Println("i uint16 size:", Sizeof(i))
	var j uint32 = 9
	fmt.Println("j uint32 size:", Sizeof(j))
	var k uint64 = 10
	fmt.Println("k uint64 size:", Sizeof(k))
	var l uintptr = uintptr(4)
	fmt.Println("l uintptr size:", Sizeof(l))
	var m float32 = 1.0
	fmt.Println("m float32 size:", Sizeof(m))
	var n float64 = 2.0
	fmt.Println("n float64 size:", Sizeof(n))

	var o complex64 = 3i + 1
	fmt.Println("o complex64 size:", Sizeof(o))
	var p complex128 = 4i + 2
	fmt.Println("p complex128 size:", Sizeof(p))
	var q func() = func() {}
	fmt.Println("q func() size:", Sizeof(q))
	var r unsafe.Pointer = unsafe.Pointer(&c)
	fmt.Println("r unsafe.Pointer size:", Sizeof(r))
	var s string = "123"
	fmt.Println("s string size:", Sizeof(s))
	var t1 interface{} = s
	fmt.Println("t interface{} size:", Sizeof(t1))
	var u *int = &b
	fmt.Println("u *int size:", Sizeof(u))
	var v = [5]int{1, 2, 3, 4, 5}
	fmt.Println("v array size:", Sizeof(v))
	var w = make([]bool, 0, 10)
	fmt.Println("w slice size:", Sizeof(w))
	var x = make(map[bool]bool)
	fmt.Println("x map size:", Sizeof(x))
	var y = make(chan bool)
	fmt.Println("y chan size:", Sizeof(y))
	var z = struct{}{}
	fmt.Println("z struct size:", Sizeof(z))

}
