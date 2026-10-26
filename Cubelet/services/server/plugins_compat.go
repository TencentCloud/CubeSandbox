// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"reflect"
	"unsafe"

	containerdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
	"github.com/containerd/plugin"
)

func serverPlugins(s *containerdserver.Server) []*plugin.Plugin {
	if s == nil {
		return nil
	}

	field := reflect.ValueOf(s).Elem().FieldByName("plugins")
	if !field.IsValid() {
		return nil
	}

	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().([]*plugin.Plugin)
}
