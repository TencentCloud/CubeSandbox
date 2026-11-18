// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"sort"
	"testing"

	"google.golang.org/grpc"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// fakeGRPCPluginInstance mirrors the shape of the containerd v2.3.x
// plugins/server/grpc serving instance: a struct whose embedded field carries
// the serving *grpc.Server (plugins/server/grpc returns grpcServer{Server:
// s, ...}).
type fakeGRPCPluginInstance struct {
	*grpc.Server
}

// fakeCubeboxService is a stand-in for a Cubelet-owned gRPC service plugin.
// The real services (CubeboxMgr, Images, Version, NBI, Snapshot, MultiMetaDB,
// ...) register under Cubelet-specific plugin types — not
// plugins.GRPCPlugin — and historically relied on the embedded containerd
// server registering every instance that implemented Register.
type fakeCubeboxService struct {
	cubeboxv1.UnimplementedCubeboxMgrServer
}

func (f *fakeCubeboxService) Register(server *grpc.Server) error {
	cubeboxv1.RegisterCubeboxMgrServer(server, f)
	return nil
}

// TestServingGRPCServerExtraction pins the contract that lets Cubelet-owned
// gRPC services survive the containerd v2.3.x move of the serving surface
// into the "grpc" ServerPlugin: the serving instance exposes its *grpc.Server
// as an embedded "Server" field, and registering a non-GRPCPlugin service on
// it makes the service visible over the socket. Regression for the review
// finding that containerd v2.3.x's grpc plugin only fans in
// plugins.GRPCPlugin-typed instances, which would otherwise leave every
// Cubelet service answering Unimplemented on /data/cubelet/cubelet.sock.
func TestServingGRPCServerExtraction(t *testing.T) {
	pluginInstance := fakeGRPCPluginInstance{Server: grpc.NewServer()}

	srv, err := servingGRPCServer(pluginInstance)
	if err != nil {
		t.Fatalf("servingGRPCServer failed on the plugin instance shape: %v", err)
	}
	if srv == nil {
		t.Fatal("servingGRPCServer returned a nil server")
	}

	// A struct without the embedded Server field must fail loudly instead of
	// silently dropping the services.
	if _, err := servingGRPCServer(struct{ X int }{X: 1}); err == nil {
		t.Fatal("servingGRPCServer must reject instances without an embedded Server field")
	}
}

func TestRegisterGRPCServicesOnServingInstance(t *testing.T) {
	srv := grpc.NewServer()

	// Before registration the served socket must not expose Cubelet services
	// (this is the regression state the review flagged).
	if _, ok := srv.GetServiceInfo()["cubelet.services.cubebox.v1.CubeboxMgr"]; ok {
		t.Fatal("CubeboxMgr must not be discoverable before registration")
	}

	services := []grpcService{&fakeCubeboxService{}}
	if err := registerGRPCServicesOn(srv, services); err != nil {
		t.Fatalf("registerGRPCServicesOn failed: %v", err)
	}

	info, ok := srv.GetServiceInfo()["cubelet.services.cubebox.v1.CubeboxMgr"]
	if !ok {
		t.Fatalf("CubeboxMgr missing from the serving instance's service info; got %v",
			keysOf(srv.GetServiceInfo()))
	}
	if len(info.Methods) == 0 {
		t.Fatal("CubeboxMgr service info carries no RPCs")
	}
}

func TestRegisterGRPCServicesOnPluginInstanceShape(t *testing.T) {
	// End-to-end through the same extraction path used by ServeGRPC: plugin
	// instance shape -> embedded serving server -> Cubelet service visible.
	pluginInstance := fakeGRPCPluginInstance{Server: grpc.NewServer()}
	srv, err := servingGRPCServer(pluginInstance)
	if err != nil {
		t.Fatalf("servingGRPCServer failed: %v", err)
	}

	if err := registerGRPCServicesOn(srv, []grpcService{&fakeCubeboxService{}}); err != nil {
		t.Fatalf("registerGRPCServicesOn failed: %v", err)
	}

	if _, ok := srv.GetServiceInfo()["cubelet.services.cubebox.v1.CubeboxMgr"]; !ok {
		t.Fatal("CubeboxMgr not registered on the extracted serving server")
	}
}

func keysOf(m map[string]grpc.ServiceInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
