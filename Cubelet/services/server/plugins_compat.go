// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"fmt"
	"reflect"
	"unsafe"

	containerdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
	"github.com/containerd/containerd/v2/plugins"
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

// containerdServerPluginInstance returns the instance of the containerd
// server plugin with the given id.
//
// containerd v2.3.x hosts the serving endpoints (grpc, ttrpc, grpc-tcp) as
// plugins.ServerPlugin instances instead of exposing ServeGRPC/ServeTTRPC/
// ServeTCP on cmd/containerd/server.Server. Cubelet keeps owning the listener
// lifecycle (creation in cmd/cubelet/main.go, serving via its own serve loop),
// so it needs the underlying serving instances rather than letting the plugins
// self-serve through Server.Start.
func containerdServerPluginInstance(s *containerdserver.Server, id string) (any, error) {
	for _, p := range serverPlugins(s) {
		if p.Registration.Type == plugins.ServerPlugin && p.Registration.ID == id {
			return p.Instance()
		}
	}
	return nil, fmt.Errorf("containerd server plugin %q not found", id)
}
