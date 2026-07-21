// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package grpcsrv implements the unified gRPC server (Controller + Node hooks).
package grpcsrv

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	vpb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/volumeplugin/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/grpctarget"
	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/cosapi"
	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/cosfs"
	"google.golang.org/grpc"
)

const driverName = "cos-rpc"

// Server implements VolumeControllerService and VolumePluginService.
type Server struct {
	vpb.UnimplementedVolumeControllerServiceServer
	vpb.UnimplementedVolumePluginServiceServer

	cos *cosapi.Client
	fs  *cosfs.Manager
}

// NewServer creates a plugin gRPC server.
func NewServer(cfg *config.Config) (*Server, error) {
	client, err := cosapi.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{cos: client, fs: cosfs.New(cfg)}, nil
}

func (s *Server) Create(ctx context.Context, req *vpb.CreateRequest) (*vpb.CreateResponse, error) {
	volumeID := req.GetVolumeId()
	name := req.GetName()
	if name == "" {
		name = volumeID
	}
	if err := s.cos.CreateVolume(ctx, volumeID); err != nil {
		return nil, fmt.Errorf("create volume %q: %w", volumeID, err)
	}
	log.Printf("[cos-rpc] create volumeID=%s name=%s", volumeID, name)
	return &vpb.CreateResponse{Token: ""}, nil
}

func (s *Server) Destroy(ctx context.Context, req *vpb.DestroyRequest) (*vpb.DestroyResponse, error) {
	volumeID := req.GetVolumeId()
	if err := s.cos.DestroyVolume(ctx, volumeID); err != nil {
		return nil, fmt.Errorf("destroy volume %q: %w", volumeID, err)
	}
	log.Printf("[cos-rpc] destroy volumeID=%s", volumeID)
	return &vpb.DestroyResponse{}, nil
}

func (s *Server) Attach(_ context.Context, req *vpb.AttachRequest) (*vpb.AttachResponse, error) {
	volumeID := req.GetVolumeId()
	baseDir := req.GetVolumeBaseDir()
	if req.GetRefCount() > 0 {
		mnt := s.fs.MountPoint(baseDir, volumeID)
		return &vpb.AttachResponse{
			HostPath: mnt,
			Metadata: map[string]string{
				"mount_dir": mnt,
				"volume_id": volumeID,
			},
		}, nil
	}

	hostPath, err := s.fs.Mount(baseDir, volumeID)
	if err != nil {
		return nil, fmt.Errorf("attach volume %q: %w", volumeID, err)
	}
	log.Printf("[cos-rpc] attach sandbox=%s volume=%s host_path=%s", req.GetSandboxId(), volumeID, hostPath)

	return &vpb.AttachResponse{
		HostPath: hostPath,
		Metadata: map[string]string{
			"mount_dir": hostPath,
			"volume_id": volumeID,
		},
	}, nil
}

func (s *Server) Detach(_ context.Context, req *vpb.DetachRequest) (*vpb.DetachResponse, error) {
	if req.GetRefCount() > 0 {
		return &vpb.DetachResponse{}, nil
	}

	volumeID := req.GetVolumeId()
	// Prefer the exact mount_dir recorded at attach; fall back to the default
	// base dir for records written by an older plugin/Cubelet.
	mnt := req.GetMetadata()["mount_dir"]
	if mnt == "" {
		mnt = s.fs.MountPoint("", volumeID)
	}
	if err := s.fs.Unmount(volumeID, mnt); err != nil {
		return nil, fmt.Errorf("detach volume %q: %w", volumeID, err)
	}
	log.Printf("[cos-rpc] detach volume=%s (COS data preserved)", volumeID)
	return &vpb.DetachResponse{}, nil
}

// Serve listens on cfg.ListenAddr and serves gRPC until ctx is cancelled.
func Serve(ctx context.Context, cfg *config.Config) error {
	network, addr := grpctarget.ParseListen(cfg.ListenAddr())
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
			return err
		}
		_ = os.Remove(addr)
	}

	lis, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	defer lis.Close()

	srv, err := NewServer(cfg)
	if err != nil {
		return err
	}

	grpcSrv := grpc.NewServer()
	vpb.RegisterVolumeControllerServiceServer(grpcSrv, srv)
	vpb.RegisterVolumePluginServiceServer(grpcSrv, srv)

	go func() {
		<-ctx.Done()
		grpcSrv.GracefulStop()
	}()

	log.Printf("[cos-rpc] listening on %s (driver=%s)", cfg.ListenAddr(), driverName)
	return grpcSrv.Serve(lis)
}
