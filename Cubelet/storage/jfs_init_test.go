// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func withJFSTestStorage(t *testing.T, l *local) {
	t.Helper()
	prev := localStorage
	localStorage = l
	t.Cleanup(func() { localStorage = prev })
}

func TestJFSStorageRootLayout(t *testing.T) {
	cfg := &Config{DataPath: "/data/cubelet/storage", StorageBackend: StorageBackendCow}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs/", Root: "/cube/", NodeName: "node-a"}
	withJFSTestStorage(t, &local{config: cfg})

	require.Equal(t, "/mnt/jfs/cube/nodes/node-a", JFSStorageRoot())
	require.Equal(t, "/mnt/jfs/cube/nodes/node-a", BackendStorageRoot(cow.BackendJFS))
	require.Equal(t, "/mnt/jfs/cube/nodes/node-a/pause-snapshots",
		SnapshotKindRoot("juicefs", SnapshotKindPause))
	require.Equal(t, "/data/cubelet/storage/xfs", BackendStorageRoot(cow.BackendXFS))
}

func TestJFSStorageRootDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.Cow.JFS = CowJFSUserConfig{NodeName: "n1"}
	require.Equal(t, filepath.Join(defaultJFSMountPath, defaultJFSRoot, jfsNodesDir, "n1"), cfg.jfsStorageRoot())
}

func TestBuildJFSCowInitJSONUsesCopyFileRange(t *testing.T) {
	cfg := &Config{}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs", NodeName: "n1"}
	raw, err := cfg.BuildJFSCowInitJSON()
	require.NoError(t, err)
	var payload struct {
		Backend struct {
			Kind    string `json:"kind"`
			Reflink struct {
				RootDir     string `json:"root_dir"`
				CloneMethod string `json:"clone_method"`
			} `json:"reflink"`
		} `json:"backend"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, cowBackendReflink, payload.Backend.Kind)
	require.Equal(t, "/mnt/jfs/cube/nodes/n1/objects", payload.Backend.Reflink.RootDir)
	require.Equal(t, cowCloneCFR, payload.Backend.Reflink.CloneMethod)
}

func TestStoreForBackendJFS(t *testing.T) {
	cfg := &Config{StorageBackend: StorageBackendCow}
	l := &local{config: cfg, cowManager: &XfsCow{engine: &cubecow.Engine{}}}
	withJFSTestStorage(t, l)

	_, err := l.storeForBackend(cow.BackendJFS)
	require.ErrorIs(t, err, ErrJFSNotConfigured)

	cfg.Cow.JFS.Enable = true
	_, err = l.storeForBackend("juicefs")
	require.ErrorIs(t, err, ErrJFSNotReady)

	jfsStore := &XfsCow{engine: &cubecow.Engine{}}
	l.jfsCowManager = jfsStore
	got, err := l.storeForBackend(cow.BackendJFS)
	require.NoError(t, err)
	require.Same(t, jfsStore, got)
	xfs, err := l.storeForBackend(cow.BackendXFS)
	require.NoError(t, err)
	require.NotSame(t, jfsStore, xfs)
}

func TestTryJFSCowInitOnceRequiresJuiceFSMount(t *testing.T) {
	cfg := &Config{StorageBackend: StorageBackendCow}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs", NodeName: "n1"}
	l := &local{config: cfg}

	prevCheck, prevInit := checkJFSMount, initJFSCowEngine
	t.Cleanup(func() { checkJFSMount, initJFSCowEngine = prevCheck, prevInit })
	mountErr := errors.New("not mounted")
	inits := 0
	checkJFSMount = func(string) error { return mountErr }
	initJFSCowEngine = func(*Config) (*cubecow.Engine, string, error) {
		inits++
		return &cubecow.Engine{}, "test", nil
	}

	require.ErrorIs(t, l.tryJFSCowInitOnce(context.Background()), mountErr)
	require.Zero(t, inits, "the handle must not open on a path that is not a JuiceFS mount")
	require.Nil(t, l.jfsCowManager)

	checkJFSMount = func(string) error { return nil }
	require.NoError(t, l.tryJFSCowInitOnce(context.Background()))
	require.Equal(t, 1, inits)
	require.NotNil(t, l.jfsCowManager)
	require.NoError(t, l.tryJFSCowInitOnce(context.Background()))
	require.Equal(t, 1, inits, "a published handle is not reopened")
}

func TestJFSCowInitCancellationDoesNotPublishHandle(t *testing.T) {
	cfg := &Config{StorageBackend: StorageBackendCow}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs", NodeName: "n1"}
	l := &local{config: cfg}

	prevCheck, prevInit := checkJFSMount, initJFSCowEngine
	t.Cleanup(func() { checkJFSMount, initJFSCowEngine = prevCheck, prevInit })
	checkJFSMount = func(string) error { return nil }
	started, release := make(chan struct{}), make(chan struct{})
	initJFSCowEngine = func(*Config) (*cubecow.Engine, string, error) {
		close(started)
		<-release
		return &cubecow.Engine{}, "test", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.tryJFSCowInitOnce(ctx) }()
	<-started
	cancel()
	close(release)
	require.ErrorIs(t, <-done, context.Canceled)
	engine, manager := l.loadJFSCowState()
	require.Nil(t, engine)
	require.Nil(t, manager)
}

func TestJFSCowConcurrentReadersObserveOnlyPublishedHandle(t *testing.T) {
	cfg := &Config{StorageBackend: StorageBackendCow}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs", NodeName: "n1"}
	l := &local{config: cfg, cowManager: &XfsCow{engine: &cubecow.Engine{}}}

	prevCheck, prevInit := checkJFSMount, initJFSCowEngine
	t.Cleanup(func() { checkJFSMount, initJFSCowEngine = prevCheck, prevInit })
	checkJFSMount = func(string) error { return nil }
	started, release := make(chan struct{}), make(chan struct{})
	initJFSCowEngine = func(*Config) (*cubecow.Engine, string, error) {
		close(started)
		<-release
		return &cubecow.Engine{}, "test", nil
	}

	done := make(chan error, 1)
	go func() { done <- l.tryJFSCowInitOnce(context.Background()) }()
	<-started
	var readers sync.WaitGroup
	readErr := make(chan error, 16)
	for range 16 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				_, err := l.storeForBackend(cow.BackendJFS)
				if !errors.Is(err, ErrJFSNotReady) {
					readErr <- err
					return
				}
			}
		}()
	}
	readers.Wait()
	close(readErr)
	for err := range readErr {
		require.ErrorIs(t, err, ErrJFSNotReady)
	}
	close(release)
	require.NoError(t, <-done)
	store, err := l.storeForBackend(cow.BackendJFS)
	require.NoError(t, err)
	require.NotNil(t, store)
}

func TestCloseJFSCowWaitsForInitializationAndClearsPublishedHandle(t *testing.T) {
	cfg := &Config{StorageBackend: StorageBackendCow}
	cfg.Cow.JFS = CowJFSUserConfig{Enable: true, MountPath: "/mnt/jfs", NodeName: "n1"}
	l := &local{config: cfg}

	prevCheck, prevInit := checkJFSMount, initJFSCowEngine
	t.Cleanup(func() { checkJFSMount, initJFSCowEngine = prevCheck, prevInit })
	checkJFSMount = func(string) error { return nil }
	started, release := make(chan struct{}), make(chan struct{})
	initJFSCowEngine = func(*Config) (*cubecow.Engine, string, error) {
		close(started)
		<-release
		return &cubecow.Engine{}, "test", nil
	}

	initDone := make(chan error, 1)
	go func() { initDone <- l.tryJFSCowInitOnce(context.Background()) }()
	<-started
	closeDone := make(chan struct{})
	go func() {
		l.closeJFSCow()
		close(closeDone)
	}()
	close(release)
	require.NoError(t, <-initDone)
	<-closeDone
	engine, manager := l.loadJFSCowState()
	require.Nil(t, engine)
	require.Nil(t, manager)
}

func TestCheckJuiceFSMountRejectsNonJuiceFS(t *testing.T) {
	require.Error(t, checkJuiceFSMount(t.TempDir()))
	require.Error(t, checkJuiceFSMount("/"))
}

func TestCheckJuiceFSMountAcceptsJuiceFSAfterBindMount(t *testing.T) {
	mounts := strings.NewReader("/dev/vdb /data/cubelet/jfs xfs rw 0 0\n" +
		"JuiceFS:cube /data/cubelet/jfs fuse.juicefs rw 0 0\n")
	require.NoError(t, checkJuiceFSMountFrom(mounts, "/data/cubelet/jfs"))
}
