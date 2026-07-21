// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	volpkg "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	volbinary "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/binary"
	volrpc "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/rpc"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var cowLookPath = exec.LookPath
var initCowEngine = initCowEngineWithConfig

// StorageBackendCow is the canonical value of `storage_backend` for the
// cubecow (reflink-only copy-on-write) backend. cubelet refuses to boot
// when `storage_backend` is set to anything else under this build.
const StorageBackendCow = "cubecow"

// cowBackendReflink is the only backend `kind` cubecow now supports.
// It is forwarded verbatim into the cubecow inline JSON payload and
// matches the `BackendKind::Reflink` variant on the Rust side.
const cowBackendReflink = "reflink"

// defaultVolumePluginBaseDir is the fallback parent directory that
// plugin_volume Attach must mount volumes under when Config.VolumePluginBaseDir
// is not set in TOML.
const defaultVolumePluginBaseDir = "/data/volume"

// reflinkExt4InitCommands lists the external commands the **cubelet
// upper layers** need when they initialise an ext4 default-medium
// volume on top of a reflink-backed file. cubecow itself uses zero
// external commands (mkdir/open/FICLONE/statvfs are pure libc) — but
// `initExt4BlockDevice` in `cubecow_volume_manager.go` formats the
// reflink file as ext4 and mounts it, which under the hood pulls in
// `losetup` (auto-loop in mount(8)). Surface those commands here so a
// missing binary is reported at startup rather than at the first
// `CreateDefaultMediumVolume` call.
var reflinkExt4InitCommands = []string{
	"mkfs.ext4",
	"mount",
	"umount",
	"losetup",
}

type Config struct {
	RootPath string `toml:"root_path"`
	DataPath string `toml:"data_path"`

	DiskSize string `toml:"disksize"`

	WarningPercent int64 `toml:"warningPercent"`

	PoolDefaultFormatSizeList []string `toml:"pool_default_format_size_list"`

	BaseDiskUUID string `toml:"base_disk_uuid"`

	PoolSize int `toml:"pool_size"`

	PoolWorkers int `toml:"pool_worker_num"`

	FAdviseSize int `toml:"fadvise_size"`

	PoolType poolType `toml:"pool_type"`

	PoolTriggerIntervalInMs int `toml:"pool_trigger_interval_in_ms"`

	PoolTriggerBurst int `toml:"pool_trigger_burst"`

	DisableDiskCheck bool `toml:"disable_disk_check"`

	FreeBlocksThreshold int32 `toml:"free_blocks_threshold"`

	FreeInodesThreshold int32            `toml:"free_inodes_threshold"`
	ReconcileInterval   tomlext.Duration `toml:"reconcile_interval"`

	StorageBackend string          `toml:"storage_backend"`
	Cow            CowInlineConfig `toml:"cow"`

	// CmdTimeout overrides the per-command timeout for utils.ExecV
	// invocations in shell.go (cp / truncate / e2fsck / resize2fs /
	// mkfs.ext4). Defaults to defaultCmdTimeout when zero. The slow
	// ext4-create path on multi-GiB images can need noticeably more
	// than the 3s default; this knob lets operators bump it without
	// recompiling.
	CmdTimeout tomlext.Duration `toml:"cmd_timeout"`

	// VolumePlugins lists external volume plugin configurations.
	// Built-in plugins are registered in code and do not need entries here.
	//
	// Example:
	//   [[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
	//     name        = "nfs"
	//     type        = "binary"
	//     binary_path = "/usr/local/bin/cube-volume-nfs"
	VolumePlugins []volpkg.PluginConfig `toml:"volume_plugins"`

	// VolumePluginBaseDir is the parent directory that every plugin_volume
	// Attach must mount its volume under. Cubelet passes this path to the
	// plugin (AttachRequest.VolumeBaseDir / --volume-base-dir) and rejects any
	// attach whose returned host_path is not located inside it. Defaults to
	// defaultVolumePluginBaseDir ("/data/volume") when empty.
	VolumePluginBaseDir string `toml:"volume_plugin_base_dir"`
}

// CowInlineConfig mirrors the cubecow `AppConfig` schema. cubecow is
// reflink-only and cubelet always owns the cubecow init payload
// (there is no external cubecow.toml fallback), so the only thing
// users can tune through TOML is the `[log]` block. The reflink
// backend's `root_dir` is derived from `data_path` and stamped onto
// `Backend.Reflink` in PrepareCowInlineConfig.
type CowInlineConfig struct {
	Log     CowLogConfig     `toml:"log"`
	Backend CowBackendConfig `toml:"-"`
}

type CowLogConfig struct {
	Level    *string `toml:"level"`
	Format   *string `toml:"format"`
	File     *string `toml:"file"`
	Rotation *string `toml:"rotation"`
}

// CowBackendConfig is filled in by cubelet at init time, never by the
// user, and shipped to cubecow as the `[backend]` block.
type CowBackendConfig struct {
	Kind    string `toml:"-"`
	Reflink CowReflinkBackendConfig
}

// CowReflinkBackendConfig is the `[backend.reflink]` payload.
// cubelet always derives `RootDir` from `data_path` so reflink files
// live on the same FICLONE-capable filesystem as the rest of
// cubelet's state.
type CowReflinkBackendConfig struct {
	RootDir *string `toml:"-"`
}

func (c *Config) BuildCowInitJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil storage config")
	}
	payload := map[string]any{}
	if logBlock := c.Cow.Log.toMap(); len(logBlock) > 0 {
		payload["log"] = logBlock
	}
	if backendBlock := c.Cow.Backend.toMap(); len(backendBlock) > 0 {
		payload["backend"] = backendBlock
	}
	return json.Marshal(payload)
}

// PrepareCowInlineConfig stamps cubelet-owned defaults (currently the
// reflink backend kind and its `root_dir` derived from `data_path`)
// onto the config so BuildCowInitJSON has everything cubecow needs.
func (c *Config) PrepareCowInlineConfig() error {
	if c == nil {
		return fmt.Errorf("nil storage config")
	}
	c.Cow.Backend.Kind = cowBackendReflink
	autoDir := defaultReflinkAutoRootDir(c.DataPath)
	c.Cow.Backend.Reflink.RootDir = &autoDir
	return nil
}

// cowReflinkRootDir returns the effective reflink root_dir for this
// deployment so cleanup/diagnostics can act on the right directory.
func (c *Config) cowReflinkRootDir() (string, error) {
	if c == nil {
		return "", fmt.Errorf("nil storage config")
	}
	return defaultReflinkAutoRootDir(c.DataPath), nil
}

// defaultReflinkAutoRootDir picks `<data_path-base>/cubecow-reflink/`
// when no explicit `root_dir` is provided. It strips the
// `<plugin>.<id>` storage suffix from `dataPath` so reflink files
// share the same physical filesystem as the rest of cubelet's
// persistent state instead of accidentally landing on the OS disk
// under cubecow's library-level fallback.
func defaultReflinkAutoRootDir(dataPath string) string {
	storageDir := fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID)
	baseDir := filepath.Clean(dataPath)
	if filepath.Base(baseDir) == storageDir {
		baseDir = filepath.Dir(baseDir)
	}
	return filepath.Join(baseDir, "cubecow-reflink")
}

func (c *Config) cowStartupCommands() []string {
	return append([]string{}, reflinkExt4InitCommands...)
}

func (c *Config) validateCowStartupDeps() error {
	cmds := c.cowStartupCommands()
	missing := make([]string, 0)
	for _, cmd := range cmds {
		if _, err := cowLookPath(cmd); err != nil {
			missing = append(missing, cmd)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"cubecow startup dependency check failed, missing commands in PATH: %s (required commands: %s)",
		strings.Join(missing, ", "),
		strings.Join(cmds, ", "),
	)
}

func initCowEngineWithConfig(cfg *Config) (*cubecow.Engine, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("nil storage config")
	}
	if err := cfg.PrepareCowInlineConfig(); err != nil {
		return nil, "", err
	}
	payload, err := cfg.BuildCowInitJSON()
	if err != nil {
		return nil, "", err
	}
	engine, err := cubecow.InitWithoutLoggingFromJSON(string(payload))
	return engine, "inline storage.cow config", err
}

func (c CowLogConfig) toMap() map[string]any {
	m := map[string]any{}
	setIfNotNil(m, "level", c.Level)
	setIfNotNil(m, "format", c.Format)
	setIfNotNil(m, "file", c.File)
	setIfNotNil(m, "rotation", c.Rotation)
	return m
}

func (c CowBackendConfig) toMap() map[string]any {
	m := map[string]any{}
	if c.Kind != "" {
		m["kind"] = c.Kind
	}
	if sub := c.Reflink.toMap(); len(sub) > 0 {
		m["reflink"] = sub
	}
	return m
}

func (c CowReflinkBackendConfig) toMap() map[string]any {
	m := map[string]any{}
	setIfNotNil(m, "root_dir", c.RootDir)
	return m
}

func setIfNotNil[T any](dst map[string]any, key string, value *T) {
	if value != nil {
		dst[key] = *value
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.StorageID.ID(),
		Config: &Config{},
		Requires: []plugin.Type{
			constants.CubeStorePlugin,
			constants.CubeMetaStorePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			localStorage.config = ic.Config.(*Config)
			if localStorage.config.RootPath == "" {
				localStorage.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}
			if localStorage.config.DataPath == "" {
				localStorage.config.DataPath = localStorage.config.RootPath
			} else {
				localStorage.config.DataPath = filepath.Join(localStorage.config.DataPath,
					fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID))
			}
			if localStorage.config.PoolType == "" {
				localStorage.config.PoolType = cp_type
			}
			if localStorage.config.VolumePluginBaseDir == "" {
				localStorage.config.VolumePluginBaseDir = defaultVolumePluginBaseDir
			}
			if localStorage.config.CmdTimeout == 0 {
				localStorage.config.CmdTimeout = tomlext.FromStdTime(defaultCmdTimeout)
			}
			if tomlext.ToStdTime(localStorage.config.CmdTimeout) < 0 {
				return nil, fmt.Errorf("cmd_timeout must be non-negative, got %v",
					tomlext.ToStdTime(localStorage.config.CmdTimeout))
			}
			cmdTimeout = tomlext.ToStdTime(localStorage.config.CmdTimeout)
			if localStorage.config.StorageBackend != StorageBackendCow {
				checkPoolType(localStorage.config)
			}
			if localStorage.config.StorageBackend == StorageBackendCow {
				if err := localStorage.config.validateCowStartupDeps(); err != nil {
					CubeLog.Errorf("plugin %s cubecow dependency check fail:%v", constants.StorageID, err)
					return nil, err
				}
				eng, initSource, err := initCowEngine(localStorage.config)
				if err != nil {
					CubeLog.Errorf("plugin %s cubecow init fail:%v", constants.StorageID, err)
					return nil, err
				}
				localStorage.cowEngine = eng
				CubeLog.Infof("cubecow engine initialized from %s", initSource)
			}

			cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("get cubebox api client fail:%v", err)
			}
			localStorage.cubeboxAPI = cubeboxAPIObj.(cubes.CubeboxAPI)
			CubeLog.Debugf("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID), localStorage.config)

			if err := localStorage.init(ic); err != nil {
				CubeLog.Errorf("plugin %s init fail:%v", constants.StorageID, err)
				return nil, err
			}

			// initialise external volume plugins declared in TOML
			if err := initVolumePlugins(ic.Context, localStorage.config); err != nil {
				CubeLog.Errorf("volume plugin init fail: %v", err)
				return nil, err
			}

			SetSnapshotCatalogRoots(constants.DefaultSnapshotDir)

			return localStorage, nil
		},
	})
}

func checkPoolType(c *Config) {
	if c.PoolType == cp_reflink_type {
		baseFormatFile := filepath.Join(c.DataPath, "base.raw")
		targetFile := filepath.Join(c.DataPath, "target.raw")
		defer func() {
			_ = os.RemoveAll(baseFormatFile)
			_ = os.RemoveAll(targetFile)
		}()
		if err := newExt4BaseRaw(baseFormatFile, c.BaseDiskUUID, 512000); err != nil {
			c.PoolType = cp_type
			return
		}

		if err := newExt4RawByReflinkCopy(baseFormatFile, targetFile, 0); err != nil {
			c.PoolType = cp_type
			return
		}
	}
}

// collectLiveSandboxIDs reads all StorageInfo entries from the local DB and
// returns the set of sandbox IDs that are currently persisted.
// collectLiveSandboxIDs returns the set of sandbox IDs that are currently
// alive according to the in-memory cubebox store.  This is authoritative:
// if a sandbox has been destroyed, its entry is gone from the store even if
// a stale StorageInfo record remains in the DB.
//
// Falls back to reading all StorageInfo entries from the DB if cubeboxAPI
// is not available (e.g. during early init).
func collectLiveSandboxIDs() (map[string]struct{}, error) {
	if api := localStorage.cubeboxAPI; api != nil {
		boxes := api.List()
		live := make(map[string]struct{}, len(boxes))
		for _, b := range boxes {
			if b != nil {
				live[b.SandboxID] = struct{}{}
			}
		}
		return live, nil
	}
	// Fallback: use storage DB (may include stale entries from failed destroys).
	all, err := localStorage.readAllFileInfo()
	if err != nil {
		return nil, fmt.Errorf("readAllFileInfo: %w", err)
	}
	live := make(map[string]struct{}, len(all))
	for k := range all {
		if k == stubKeyName {
			continue
		}
		live[k] = struct{}{}
	}
	return live, nil
}

// initVolumePlugins registers binary and RPC plugins declared in TOML config,
// attaches the persistent RefCountStore from localStorage to the global Manager,
// runs a recovery pass to reconcile ref-counts against live sandboxes, and
// then calls InitAll so every registered plugin receives its PluginConfig.
func initVolumePlugins(ctx context.Context, cfg *Config) error {
	mgr := volpkg.Global()
	cfgByName := make(map[string]volpkg.PluginConfig, len(cfg.VolumePlugins))
	seen := make(map[string]volpkg.PluginType, len(cfg.VolumePlugins))

	for _, pc := range cfg.VolumePlugins {
		if pc.Name == "" {
			return fmt.Errorf("volume_plugins entry has empty name")
		}
		if prev, dup := seen[pc.Name]; dup {
			return fmt.Errorf(
				"volume plugin %q: duplicate driver name (already declared as type %q); "+
					"each plugin must have a unique name because the SDK selects plugins by driver only",
				pc.Name, prev,
			)
		}
		seen[pc.Name] = pc.Type
		cfgByName[pc.Name] = pc

		if pc.Type == volpkg.PluginTypeBuiltin {
			continue
		}
		switch pc.Type {
		case volpkg.PluginTypeBinary:
			mgr.Register(volbinary.New(pc.Name))
		case volpkg.PluginTypeRPC:
			mgr.Register(volrpc.New(pc.Name))
		default:
			return fmt.Errorf("volume plugin %q: unknown type %q (want builtin|binary|rpc)", pc.Name, pc.Type)
		}
	}

	if localStorage.rcStore != nil {
		mgr.SetRefCountStore(localStorage.rcStore)

		liveIDs, err := collectLiveSandboxIDs()
		if err != nil {
			CubeLog.Warnf("[plugin_volume] refcount recovery: collect live sandboxes: %v", err)
		} else {
			res, err := localStorage.rcStore.RecoverRefCounts(liveIDs)
			if err != nil {
				CubeLog.Warnf("[plugin_volume] refcount recovery: %v", err)
			} else {
				CubeLog.Infof("[plugin_volume] refcount recovery: scanned=%d stale_removed=%d records_deleted=%d",
					res.RecordsScanned, res.StaleRefsRemoved, res.RecordsDeleted)
			}
		}
	}

	if err := mgr.InitAll(ctx, cfgByName); err != nil {
		return err
	}
	for _, pc := range cfg.VolumePlugins {
		switch pc.Type {
		case volpkg.PluginTypeBuiltin:
			continue
		case volpkg.PluginTypeBinary:
			CubeLog.Infof("[plugin_volume] initialized binary plugin %q at %s", pc.Name, pc.BinaryPath)
		case volpkg.PluginTypeRPC:
			CubeLog.Infof("[plugin_volume] initialized rpc plugin %q at %s", pc.Name, pc.SocketPath)
		}
	}
	return nil
}
