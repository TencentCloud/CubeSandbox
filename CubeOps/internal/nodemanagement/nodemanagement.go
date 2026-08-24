// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package nodemanagement wires the node domain into CubeOps.
package nodemanagement

import (
	"context"

	"gorm.io/gorm"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// Service exposes the node domain service for handlers and CLI.
type Service struct {
	*service.NodeService
}

// DeclaredVersionInfo describes expected component versions from the release manifest.
type DeclaredVersionInfo = service.DeclaredVersionInfo

// DefaultDeclaredVersionInfo returns an empty declaration.
func DefaultDeclaredVersionInfo() DeclaredVersionInfo {
	return DeclaredVersionInfo{
		Primary: map[string]string{},
		Sets:    map[string]map[string]struct{}{},
	}
}

// New creates and initialises the node domain service.
func New(ctx context.Context, db *gorm.DB, declared DeclaredVersionInfo) (*Service, error) {
	svc := service.NewNodeServiceWithHostMeta(
		store.NewNodeStore(db),
		store.NewHostMetaLoader(db),
		declared,
	)
	if err := svc.Init(ctx); err != nil {
		return nil, err
	}
	return &Service{NodeService: svc}, nil
}

// SandboxInventoryChecker is the deletion-flow sandbox counter; deploy
// *cubemaster.Client via Service.SetSandboxInventoryChecker.
type SandboxInventoryChecker = service.SandboxInventoryChecker
