// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
)

type typedNilService struct{}

func (s *typedNilService) EnsureNetwork(ctx context.Context, req *service.EnsureNetworkRequest) (*service.EnsureNetworkResponse, error) {
	return nil, nil
}

func (s *typedNilService) ReleaseNetwork(ctx context.Context, req *service.ReleaseNetworkRequest) (*service.ReleaseNetworkResponse, error) {
	return nil, nil
}

func (s *typedNilService) ReconcileNetwork(ctx context.Context, req *service.ReconcileNetworkRequest) (*service.ReconcileNetworkResponse, error) {
	return nil, nil
}

func (s *typedNilService) GetNetwork(ctx context.Context, req *service.GetNetworkRequest) (*service.GetNetworkResponse, error) {
	return nil, nil
}

func (s *typedNilService) ListNetworks(ctx context.Context, req *service.ListNetworksRequest) (*service.ListNetworksResponse, error) {
	return nil, nil
}

func (s *typedNilService) Health(ctx context.Context) error {
	return nil
}

func TestInitServiceRejectsNilFactoryResult(t *testing.T) {
	orig := newLocalService
	t.Cleanup(func() {
		newLocalService = orig
	})

	tests := []struct {
		name      string
		factory   func(service.Config) (service.Service, error)
		wantError string
	}{
		{
			name: "nil interface",
			factory: func(service.Config) (service.Service, error) {
				return nil, nil
			},
			wantError: "nil service",
		},
		{
			name: "typed nil interface",
			factory: func(service.Config) (service.Service, error) {
				var svc *typedNilService
				return svc, nil
			},
			wantError: "typed nil service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newLocalService = tt.factory

			svc, err := initService(service.DefaultConfig())
			if err == nil {
				t.Fatal("expected initService to fail")
			}
			if svc != nil {
				t.Fatal("expected returned service to be nil")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error to contain %q, got %v", tt.wantError, err)
			}
		})
	}
}
