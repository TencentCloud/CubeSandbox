// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// scheduler-plugin is a minimal external Filter+Score plugin example. It keeps
// nodes with fewer than eight in-flight creates and scores lower CPU usage
// higher. Run with SOCKET=/run/cube-scheduler-example.sock.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
	"google.golang.org/grpc"
)

const protocolVersion = "v1"

// maxSnapshots bounds how many snapshot versions are kept. The CubeMaster
// client assigns a fresh version per scheduling attempt, and concurrent
// attempts interleave SyncSnapshot with Filter/Score, so snapshots must be
// keyed by version — a single "latest" slot corrupts in-flight requests.
const maxSnapshots = 8

type server struct {
	schedulerplugin.UnimplementedSchedulerPluginServer
	mu        sync.RWMutex
	snapshots map[string]map[string]*schedulerplugin.SnapshotNode
	order     []string // FIFO of snapshot versions for eviction
}

func (s *server) Handshake(_ context.Context, request *schedulerplugin.HandshakeRequest) (*schedulerplugin.HandshakeResponse, error) {
	if request.GetProtocolVersion() != protocolVersion {
		return nil, fmt.Errorf("unsupported protocol version %q", request.GetProtocolVersion())
	}
	return &schedulerplugin.HandshakeResponse{
		ProtocolVersion: protocolVersion,
		PluginName:      request.GetPluginName(),
		Capabilities:    []string{"filter", "score"},
	}, nil
}

func (s *server) SyncSnapshot(_ context.Context, request *schedulerplugin.SnapshotRequest) (*schedulerplugin.SnapshotResponse, error) {
	nodes := make(map[string]*schedulerplugin.SnapshotNode, len(request.GetNodes()))
	for _, candidate := range request.GetNodes() {
		nodes[candidate.GetId()] = candidate
	}
	version := request.GetSnapshotVersion()
	s.mu.Lock()
	if _, exists := s.snapshots[version]; !exists {
		s.order = append(s.order, version)
		for len(s.order) > maxSnapshots {
			delete(s.snapshots, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.snapshots[version] = nodes
	s.mu.Unlock()
	return &schedulerplugin.SnapshotResponse{SnapshotVersion: version}, nil
}

func (s *server) snapshot(version string) (map[string]*schedulerplugin.SnapshotNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodes, ok := s.snapshots[version]
	if !ok {
		return nil, fmt.Errorf("snapshot %q is not synchronized or has been evicted", version)
	}
	return nodes, nil
}

func (s *server) Filter(_ context.Context, request *schedulerplugin.FilterRequest) (*schedulerplugin.FilterResponse, error) {
	nodes, err := s.snapshot(request.GetSnapshotVersion())
	if err != nil {
		return nil, err
	}
	response := &schedulerplugin.FilterResponse{SnapshotVersion: request.GetSnapshotVersion()}
	for _, id := range request.GetCandidateIds() {
		candidate := nodes[id]
		if candidate != nil && candidate.GetCreating() < 8 {
			response.KeptIds = append(response.KeptIds, id)
		}
	}
	return response, nil
}

func (s *server) Score(_ context.Context, request *schedulerplugin.ScoreRequest) (*schedulerplugin.ScoreResponse, error) {
	nodes, err := s.snapshot(request.GetSnapshotVersion())
	if err != nil {
		return nil, err
	}
	response := &schedulerplugin.ScoreResponse{SnapshotVersion: request.GetSnapshotVersion()}
	for _, id := range request.GetCandidateIds() {
		candidate := nodes[id]
		if candidate == nil {
			return nil, fmt.Errorf("unknown candidate %q", id)
		}
		value := 100 - candidate.GetCpuUtil()
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		response.Scores = append(response.Scores, &schedulerplugin.NodeScore{NodeId: id, Score: value})
	}
	return response, nil
}

func main() {
	socket := os.Getenv("SOCKET")
	if socket == "" {
		socket = "/run/cube-scheduler-example.sock"
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		log.Fatalf("listen on %s: %v", socket, err)
	}
	grpcServer := grpc.NewServer()
	schedulerplugin.RegisterSchedulerPluginServer(grpcServer, &server{
		snapshots: make(map[string]map[string]*schedulerplugin.SnapshotNode),
	})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		grpcServer.GracefulStop()
	}()
	log.Printf("scheduler plugin listening on %s", socket)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
