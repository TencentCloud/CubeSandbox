// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	defaultJFSMountPath = "/data/cubelet/jfs"
	defaultJFSCacheDir  = "/data/cubelet/jfs-cache"
	defaultJFSRoot      = "cube"
	// jfsNodesDir keeps every node's packages apart on the shared volume, so
	// one node's object scan and GC never see another node's files.
	jfsNodesDir     = "nodes"
	jfsFSTypePrefix = "fuse.juicefs"
	cowCloneCFR     = "copy_file_range"
)

// ErrJFSNotConfigured is returned for backend=jfs requests when [cow.jfs]
// enable is false.
var ErrJFSNotConfigured = errors.New("jfs storage is not configured (set [cow.jfs] enable = true)")

// ErrJFSNotReady is returned for backend=jfs requests until the JuiceFS mount
// is up and the jfs cubecow handle has opened on it.
var ErrJFSNotReady = errors.New("jfs storage is not ready (waiting for the JuiceFS mount)")

var (
	jfsInitRetryInterval = 5 * time.Second
	initJFSCowEngine     = initJFSCowEngineWithConfig
	checkJFSMount        = checkJuiceFSMount
)

func (c *Config) jfsConfigured() bool {
	return c != nil && c.Cow.JFS.Enable
}

func (c *Config) jfsMountPath() string {
	if c != nil {
		if p := strings.TrimSpace(c.Cow.JFS.MountPath); p != "" {
			return filepath.Clean(p)
		}
	}
	return defaultJFSMountPath
}

func (c *Config) jfsNodeName() string {
	if c != nil {
		if n := strings.TrimSpace(c.Cow.JFS.NodeName); n != "" {
			return n
		}
	}
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return "localhost"
}

// jfsCacheDirs are the juicefs --cache-dir entries (colon separated).
func (c *Config) jfsCacheDirs() []string {
	raw := defaultJFSCacheDir
	if c != nil {
		if d := strings.TrimSpace(c.Cow.JFS.CacheDir); d != "" {
			raw = d
		}
	}
	var dirs []string
	for _, d := range strings.Split(raw, ":") {
		if d = strings.TrimSpace(d); d != "" {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return dirs
}

// jfsNodesRoot is <mount_path>/<root>/nodes: every node's tree, and the only
// place a cross-node import may read from.
func (c *Config) jfsNodesRoot() string {
	root := defaultJFSRoot
	if c != nil {
		if r := strings.Trim(strings.TrimSpace(c.Cow.JFS.Root), "/"); r != "" {
			root = r
		}
	}
	return filepath.Join(c.jfsMountPath(), root, jfsNodesDir)
}

// jfsStorageRoot is <mount_path>/<root>/nodes/<node_name>: the jfs
// counterpart of <work>/xfs, holding objects/, snapshots/ and
// pause-snapshots/.
func (c *Config) jfsStorageRoot() string {
	return filepath.Join(c.jfsNodesRoot(), c.jfsNodeName())
}

// JFSConfigured reports whether this node has the jfs backend enabled.
func JFSConfigured() bool {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	return cfg.jfsConfigured()
}

// JFSStorageRoot is this node's jfs backend root on the JuiceFS mount.
func JFSStorageRoot() string {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	return cfg.jfsStorageRoot()
}

func (c *Config) jfsBackendConfig() CowBackendConfig {
	rootDir := filepath.Join(c.jfsStorageRoot(), SnapshotObjectsDir)
	method := cowCloneCFR
	return CowBackendConfig{
		Kind: cowBackendReflink,
		Reflink: CowReflinkBackendConfig{
			RootDir:     &rootDir,
			CloneMethod: &method,
		},
	}
}

func (c *Config) BuildJFSCowInitJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil storage config")
	}
	return c.buildCowInitJSON(c.jfsBackendConfig())
}

func initJFSCowEngineWithConfig(cfg *Config) (*cubecow.Engine, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("nil storage config")
	}
	payload, err := cfg.BuildJFSCowInitJSON()
	if err != nil {
		return nil, "", err
	}
	engine, err := cubecow.InitWithoutLoggingFromJSON(string(payload))
	return engine, "inline storage.cow jfs handle", err
}

// checkJuiceFSMount fails unless mountPath is itself a JuiceFS mount point.
// Anywhere else copy_file_range is a full data copy, and an unmounted path
// would silently put jfs packages on the local disk.
func checkJuiceFSMount(mountPath string) error {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return err
	}
	defer f.Close()
	return checkJuiceFSMountFrom(f, mountPath)
}

func checkJuiceFSMountFrom(r io.Reader, mountPath string) error {
	mountPath = filepath.Clean(mountPath)
	var foundTypes []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || filepath.Clean(fields[1]) != mountPath {
			continue
		}
		if strings.HasPrefix(fields[2], jfsFSTypePrefix) {
			return nil
		}
		foundTypes = append(foundTypes, fields[2])
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(foundTypes) > 0 {
		return fmt.Errorf("%s is mounted as %s, not %s", mountPath, strings.Join(foundTypes, ", "), jfsFSTypePrefix)
	}
	return fmt.Errorf("%s is not a mount point", mountPath)
}

// loadJFSCowState returns one consistent snapshot of the published jfs handle.
func (l *local) loadJFSCowState() (*cubecow.Engine, cowVolumeManager) {
	if l == nil {
		return nil, nil
	}
	l.jfsStateMu.RLock()
	defer l.jfsStateMu.RUnlock()
	return l.jfsCowEngine, l.jfsCowManager
}

// tryJFSCowInitOnce opens the jfs cubecow handle on the JuiceFS mount and
// publishes it. Initialization and close are serialized; requests observe
// either the complete handle or ErrJFSNotReady, never a partially-published
// interface value. A no-op once published.
func (l *local) tryJFSCowInitOnce(ctx context.Context) error {
	if l == nil || l.config == nil {
		return fmt.Errorf("storage is not initialized")
	}
	l.jfsInitMu.Lock()
	defer l.jfsInitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, manager := l.loadJFSCowState(); manager != nil {
		return nil
	}
	if err := checkJFSMount(l.config.jfsMountPath()); err != nil {
		return err
	}
	engine, source, err := initJFSCowEngine(l.config)
	if err != nil {
		return fmt.Errorf("jfs cubecow handle init: %w", err)
	}
	if err := ctx.Err(); err != nil {
		engine.Close()
		return err
	}
	manager := newCowVolumeManager(engine)
	l.jfsStateMu.Lock()
	l.jfsCowEngine = engine
	l.jfsCowManager = manager
	l.jfsStateMu.Unlock()
	CubeLog.Infof("cubecow jfs handle initialized from %s at %s", source, l.config.jfsStorageRoot())
	return nil
}

// initJFSCow opens the jfs handle before boot recovery when the mount is
// already up, and otherwise keeps retrying in the background: the mount unit
// may start after cubelet.
func (l *local) initJFSCow(parent context.Context) {
	if l == nil || !l.useCowStorage() || !l.config.jfsConfigured() {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	l.jfsInitMu.Lock()
	if l.jfsInitCancel != nil {
		l.jfsInitCancel()
	}
	l.jfsInitCancel = cancel
	l.jfsInitMu.Unlock()
	err := l.tryJFSCowInitOnce(ctx)
	if err == nil {
		return
	}
	CubeLog.Warnf("jfs cubecow init deferred; jfs requests fail with %v until it succeeds: %v", ErrJFSNotReady, err)
	go func() {
		for attempt := 2; ; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(jfsInitRetryInterval):
			}
			if err := l.tryJFSCowInitOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				CubeLog.Errorf("jfs cubecow init attempt %d fail (retry in %s): %v", attempt, jfsInitRetryInterval, err)
				continue
			}
			CubeLog.Warnf("cubecow jfs handle ready after %d attempt(s); jfs requests now served", attempt)
			return
		}
	}()
}

func (l *local) closeJFSCow() {
	if l == nil {
		return
	}
	l.jfsInitMu.Lock()
	defer l.jfsInitMu.Unlock()
	if l.jfsInitCancel != nil {
		l.jfsInitCancel()
		l.jfsInitCancel = nil
	}
	l.jfsStateMu.Lock()
	engine := l.jfsCowEngine
	l.jfsCowEngine = nil
	l.jfsCowManager = nil
	l.jfsStateMu.Unlock()
	if engine != nil {
		engine.Close()
	}
}

func isJFSBackend(backend string) bool {
	normalized, err := cow.NormalizeBackend(backend)
	return err == nil && normalized == cow.BackendJFS
}
