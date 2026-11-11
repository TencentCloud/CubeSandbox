// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

func TestNewPartialDialFailureClosesCreatedConns(t *testing.T) {
	calls := 0
	var first *grpc.ClientConn

	dial := func(ua, address string) (*grpc.ClientConn, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("dial failure")
		}
		cc, err := grpc.Dial(testTarget, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		first = cc
		return cc, nil
	}
	opt := Options{
		Dial:                 dial,
		MaxIdle:              2,
		MaxActive:            2,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}

	if _, err := New("ua", "addr", opt); err == nil {
		t.Fatal("expected New to fail")
	}
	if first == nil {
		t.Fatal("first dial was not called")
	}
	if first.GetState() != connectivity.Shutdown {
		t.Fatalf("created connection was not closed: %v", first.GetState())
	}
}

func TestNewNilDialResult(t *testing.T) {
	opt := Options{
		Dial:       func(ua, address string) (*grpc.ClientConn, error) { return nil, nil },
		OptionType: SingleConn,
		MaxIdle:    1,
		MaxActive:  1,
		Reuse:      true,
	}
	if _, err := New("ua", "addr", opt); !errors.Is(err, errNilDialConn) {
		t.Fatalf("expected errNilDialConn, got %v", err)
	}
}

func TestNewSingleConnLargerMaxActive(t *testing.T) {
	opt := Options{
		Dial: func(ua, address string) (*grpc.ClientConn, error) {
			return grpc.Dial(testTarget, grpc.WithInsecure())
		},
		OptionType: SingleConn,
		MaxIdle:    1,
		MaxActive:  4,
		Reuse:      true,
	}
	p, err := New("ua", "addr", opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	dial := func(ua, address string) (*grpc.ClientConn, error) {
		return grpc.Dial(testTarget, grpc.WithInsecure())
	}
	tests := []struct {
		name    string
		address string
		opt     Options
	}{
		{"empty address", "", Options{Dial: dial, MaxIdle: 1, MaxActive: 1, Reuse: true}},
		{"nil dial", "addr", Options{MaxIdle: 1, MaxActive: 1, Reuse: true}},
		{"zero max idle", "addr", Options{Dial: dial, MaxIdle: 0, MaxActive: 1, Reuse: true}},
		{"max idle above active", "addr", Options{Dial: dial, MaxIdle: 2, MaxActive: 1, Reuse: true}},
		{"connpool zero streams", "addr", Options{
			Dial: dial, OptionType: ConnPool, MaxIdle: 1, MaxActive: 1,
			MaxConcurrentStreams: 0, Reuse: true,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New("ua", tt.address, tt.opt); err == nil {
				t.Fatal("expected New to fail")
			}
		})
	}
}
