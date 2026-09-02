// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func TestS3lvolConfigured(t *testing.T) {
	assert.False(t, (*Config)(nil).s3lvolConfigured())
	assert.False(t, (&Config{}).s3lvolConfigured())
	assert.False(t, (&Config{Cow: CowInlineConfig{S3: CowS3UserConfig{
		Enable:     false,
		SocketPath: stringPtr("/var/run/s3lvol.sock"),
	}}}).s3lvolConfigured())
	assert.True(t, s3OptInConfig().s3lvolConfigured())
}

func s3OptInConfig() *Config {
	return &Config{
		StorageBackend: "cubecow",
		Cow: CowInlineConfig{
			S3: CowS3UserConfig{
				Enable:     true,
				SocketPath: stringPtr("/var/run/s3lvol.sock"),
			},
		},
	}
}

func TestStoreForS3NotConfiguredWhenDisabled(t *testing.T) {
	prev := localStorage
	cfg := &Config{StorageBackend: "cubecow"}
	localStorage = &local{config: cfg, cowManager: &XfsCow{engine: &cubecow.Engine{}}}
	t.Cleanup(func() {
		localStorage.stopS3CowInitLoop()
		localStorage = prev
	})

	_, err := StoreFor(cow.BackendS3)
	require.ErrorIs(t, err, ErrS3NotConfigured)

	xfs, err := StoreFor(cow.BackendXFS)
	require.NoError(t, err)
	require.NotNil(t, xfs)
}

func TestStoreForS3NotReadyUntilInit(t *testing.T) {
	prev := localStorage
	cfg := s3OptInConfig()
	localStorage = &local{config: cfg, cowManager: &XfsCow{engine: &cubecow.Engine{}}}
	t.Cleanup(func() {
		localStorage.stopS3CowInitLoop()
		localStorage = prev
	})

	_, err := StoreFor(cow.BackendS3)
	require.ErrorIs(t, err, ErrS3NotReady)

	xfs, err := StoreFor(cow.BackendXFS)
	require.NoError(t, err)
	require.NotNil(t, xfs)
}

func TestTryS3CowInitOncePublishesAfterMetadata(t *testing.T) {
	prev := localStorage
	cfg := s3OptInConfig()
	s := &local{config: cfg}
	localStorage = s
	t.Cleanup(func() {
		s.clearS3Cow()
		localStorage = prev
		initS3CowEngine = initS3CowEngineWithConfig
		ensureS3MetadataReadyFn = EnsureS3MetadataReady
	})

	eng := &cubecow.Engine{}
	initS3CowEngine = func(got *Config) (*cubecow.Engine, string, error) {
		assert.Same(t, cfg, got)
		return eng, "test", nil
	}
	ensureS3MetadataReadyFn = func(context.Context) error { return nil }

	require.NoError(t, s.tryS3CowInitOnce(context.Background()))
	assert.Same(t, eng, s.s3CowEngine)
	require.NotNil(t, s.s3CowManager)

	store, err := StoreFor(cow.BackendS3)
	require.NoError(t, err)
	assert.Same(t, s.s3CowManager, store)
}

func TestTryS3CowInitOnceDoesNotPublishOnMetadataFail(t *testing.T) {
	prev := localStorage
	cfg := s3OptInConfig()
	s := &local{config: cfg}
	localStorage = s
	t.Cleanup(func() {
		s.clearS3Cow()
		localStorage = prev
		initS3CowEngine = initS3CowEngineWithConfig
		ensureS3MetadataReadyFn = EnsureS3MetadataReady
	})

	initS3CowEngine = func(*Config) (*cubecow.Engine, string, error) {
		return &cubecow.Engine{}, "test", nil
	}
	ensureS3MetadataReadyFn = func(context.Context) error {
		return errors.New("metadata base device path is required")
	}

	err := s.tryS3CowInitOnce(context.Background())
	require.Error(t, err)
	assert.Nil(t, s.s3CowEngine)
	assert.Nil(t, s.s3CowManager)

	_, storeErr := StoreFor(cow.BackendS3)
	require.ErrorIs(t, storeErr, ErrS3NotReady)
}

func TestS3CowInitLoopRetriesUntilSuccess(t *testing.T) {
	prev := localStorage
	prevInterval := s3InitRetryInterval
	cfg := s3OptInConfig()
	s := &local{config: cfg}
	localStorage = s
	s3InitRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		s.stopS3CowInitLoop()
		s.clearS3Cow()
		localStorage = prev
		s3InitRetryInterval = prevInterval
		initS3CowEngine = initS3CowEngineWithConfig
		ensureS3MetadataReadyFn = EnsureS3MetadataReady
	})

	attempts := 0
	eng := &cubecow.Engine{}
	initS3CowEngine = func(*Config) (*cubecow.Engine, string, error) {
		attempts++
		if attempts < 3 {
			return nil, "", errors.New("s3lvol not up")
		}
		return eng, "test", nil
	}
	ensureS3MetadataReadyFn = func(context.Context) error { return nil }

	s.startS3CowInitLoop(context.Background())
	require.Eventually(t, func() bool {
		return s.s3CowEngine == eng && s.s3CowManager != nil
	}, 2*time.Second, 20*time.Millisecond)
	assert.GreaterOrEqual(t, attempts, 3)
}

func TestS3CowInitLoopSkippedWhenDisabled(t *testing.T) {
	prev := localStorage
	cfg := &Config{
		StorageBackend: "cubecow",
		Cow: CowInlineConfig{
			S3: CowS3UserConfig{
				Enable:     false,
				SocketPath: stringPtr("/var/run/s3lvol.sock"),
			},
		},
	}
	s := &local{config: cfg}
	localStorage = s
	t.Cleanup(func() {
		s.stopS3CowInitLoop()
		s.clearS3Cow()
		localStorage = prev
		initS3CowEngine = initS3CowEngineWithConfig
	})

	attempts := 0
	initS3CowEngine = func(*Config) (*cubecow.Engine, string, error) {
		attempts++
		return nil, "", errors.New("must not be called")
	}

	s.startS3CowInitLoop(context.Background())
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, attempts)
	assert.Nil(t, s.s3InitCancel)
	_, err := StoreFor(cow.BackendS3)
	require.ErrorIs(t, err, ErrS3NotConfigured)
}
