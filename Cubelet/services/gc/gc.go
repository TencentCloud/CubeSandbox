// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin/registry"
	jsoniter "github.com/json-iterator/go"
	"github.com/moby/sys/mountinfo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type GCConfig struct {
	RootPath string `toml:"root_path"`
}

var l = &local{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.GCID.ID(),
		Config: &GCConfig{},
		Requires: []plugin.Type{
			constants.CubeStorePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					CubeLog.Fatalf("plugin %s init fail:%v", constants.GCID, err.Error())
				}
			}()
			config := ic.Config.(*GCConfig)
			if config.RootPath == "" {
				config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			if err := os.MkdirAll(path.Clean(config.RootPath), os.ModeDir|0755); err != nil {
				return nil, fmt.Errorf("init RootPath dir failed, %s", err.Error())
			}
			l.config = config

			if err := l.initDb(); err != nil {
				return nil, err
			}

			cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("get cubebox api client fail:%v", err)
			}
			l.cubeboxManger = cubeboxAPIObj.(cubes.CubeboxAPI)
			return l, nil
		},
	})
}

type local struct {
	config        *GCConfig
	db            *utils.CubeStore
	cubeboxManger cubes.CubeboxAPI
	metadataMu    sync.Mutex
}

var (
	dbDir      = "db"
	bucketName = "sandbox/v1"

	registerBucket = multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     bucketName,
			DbName:   "gcservice",
			Describe: "gc service db to store sandbox info",
		},
	}
)

func (l *local) initDb() error {
	basePath := filepath.Join(l.config.RootPath, dbDir)
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}
	var err error
	if l.db, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}

	registerBucket.CubeStore = l.db
	multimeta.RegisterBucket(&registerBucket)
	return nil
}

func (l *local) ID() string {
	return constants.GCID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log.G(ctx).Errorf("Init doing")
	defer log.G(ctx).Errorf("Init end")
	_ = l.db.Close()
	time.Sleep(time.Second)

	_ = mount.UnmountAll(l.config.RootPath, 0)
	if err := os.RemoveAll(path.Clean(l.config.RootPath)); err != nil {
		log.G(ctx).Infof("init fail,RemoveAll err:%v", err)
		return err
	}

	if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init RootPath dir failed, %s", err.Error())
	}

	size := 100
	m := &mount.Mount{
		Type:    "tmpfs",
		Source:  "none",
		Options: []string{fmt.Sprintf("size=%dm", size)},
	}
	if err := m.Mount(l.config.RootPath); err != nil {
		return err
	}
	exist, _ := mountinfo.Mounted(l.config.RootPath)
	if !exist {
		return fmt.Errorf("mount tmpfs:%v fail", l.config.RootPath)
	}

	if err := l.initDb(); err != nil {
		return err
	}
	return nil
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	log.G(ctx).Errorf("Create doing")
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	info := &sandBoxInfo{
		SandboxID: opts.SandboxID,
		Namespace: ns,
	}
	if err := l.createSandBoxInfo(info); err != nil {
		log.G(ctx).Warnf("saveSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}

	cb, err := l.cubeboxManger.Get(ctx, opts.SandboxID)
	if err == nil {
		if cb.UserMarkDeletedTime == nil {
			now := time.Now()
			cb.UserMarkDeletedTime = &now
			l.cubeboxManger.SyncByID(ctx, opts.SandboxID)
		}
	}
	return nil
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.Destroy nil")
	}
	log.G(ctx).Debugf("Destroy doing")
	if err := l.deleteSandBoxInfo(opts.SandboxID); err != nil {
		log.G(ctx).Warnf("deleteSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}
	log.G(ctx).Errorf("CleanUp doing")
	if err := l.deleteSandBoxInfo(opts.SandboxID); err != nil {
		log.G(ctx).Errorf("deleteSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

type sandBoxInfo struct {
	SandboxID string
	Namespace string
	// Attempts counts cleanup rounds that have already failed. Cleanup is
	// idempotent and most failures are transient, so retrying is right — up to
	// a point. Past it the failure is structural, and retrying every 5s
	// forever only keeps it invisible.
	Attempts int `json:",omitempty"`
	// FirstFailedAt is when this sandbox first failed to clean up, so an
	// operator can see how long it has been stuck rather than only how many
	// times it has been tried.
	FirstFailedAt time.Time `json:",omitempty"`
	// LastFailedAt paces the slow retry a quarantined sandbox still gets.
	LastFailedAt time.Time `json:",omitempty"`
	// QuarantinedAt marks a sandbox that has exhausted its retry budget.
	//
	// Quarantine is NOT a resolution. The sandbox's tap, IP and volumes are
	// still held by whatever refused to exit, and pool capacity stays reduced
	// until an operator reclaims them by hand. What it changes is the pace and
	// the noise: retries drop to quarantineRetryInterval and the alert fires
	// once instead of every few seconds.
	//
	// Retrying does not stop altogether, because the recovery path ends with a
	// human killing the holder — and that has to be enough on its own. If
	// quarantine were final, every manual reclaim would need a second step to
	// undo it, which is exactly the kind of thing that gets forgotten.
	QuarantinedAt *time.Time `json:",omitempty"`
}

func (i *sandBoxInfo) quarantined() bool {
	return i != nil && i.QuarantinedAt != nil
}

// dueForRetry reports whether this sandbox should be attempted in this round.
func (i *sandBoxInfo) dueForRetry(quarantineInterval time.Duration) bool {
	return i.dueForRetryAt(time.Now(), quarantineInterval)
}

func (i *sandBoxInfo) dueForRetryAt(now time.Time, quarantineInterval time.Duration) bool {
	if !i.quarantined() {
		return true
	}
	lastFailure := i.LastFailedAt
	if lastFailure.IsZero() && i.QuarantinedAt != nil {
		lastFailure = *i.QuarantinedAt
	}
	return lastFailure.IsZero() || !now.Before(lastFailure.Add(quarantineInterval))
}

func (l *local) saveSandBoxInfo(info *sandBoxInfo) error {
	b, _ := jsoniter.Marshal(info)
	return l.db.Set(bucketName, info.SandboxID, b)
}

func (l *local) readSandBoxInfo(sandBoxID string) (*sandBoxInfo, error) {
	raw, err := l.db.Get(bucketName, sandBoxID)
	if err != nil {
		return nil, err
	}
	info := &sandBoxInfo{}
	if err := jsoniter.Unmarshal(raw, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (l *local) createSandBoxInfo(info *sandBoxInfo) error {
	l.metadataMu.Lock()
	defer l.metadataMu.Unlock()

	// A destroy that keeps failing re-enters the sandbox through Create.
	// Carry the complete retry state over so neither the retry budget nor the
	// quarantine pacing is reset by an external destroy retry.
	prev, err := l.readSandBoxInfo(info.SandboxID)
	switch err {
	case nil:
		info.Attempts = prev.Attempts
		info.FirstFailedAt = prev.FirstFailedAt
		info.LastFailedAt = prev.LastFailedAt
		info.QuarantinedAt = prev.QuarantinedAt
	case utils.ErrorKeyNotFound, utils.ErrorBucketNotFound:
		// First insertion.
	default:
		return err
	}
	return l.saveSandBoxInfo(info)
}

// recordCleanupFailure bumps the failure count for a sandbox and reports
// whether that pushed it into quarantine for the first time.
func (l *local) recordCleanupFailure(sandBoxID string, maxAttempts int) (*sandBoxInfo, bool, error) {
	l.metadataMu.Lock()
	defer l.metadataMu.Unlock()

	info, err := l.readSandBoxInfo(sandBoxID)
	if err != nil {
		return nil, false, err
	}
	now := time.Now()
	info.LastFailedAt = now
	if info.FirstFailedAt.IsZero() {
		info.FirstFailedAt = now
	}
	if info.quarantined() {
		// Already reported. Keep the timestamp fresh so the slow retry stays
		// slow, and do not raise the alert again.
		return info, false, l.saveSandBoxInfo(info)
	}
	info.Attempts++
	justQuarantined := maxAttempts > 0 && info.Attempts >= maxAttempts
	if justQuarantined {
		info.QuarantinedAt = &now
	}
	if err := l.saveSandBoxInfo(info); err != nil {
		return nil, false, err
	}
	if justQuarantined {
		quarantinedSandbox.Inc()
	}
	return info, justQuarantined, nil
}

// countQuarantined is used to seed the gauge at startup, so a restart does not
// make the pool capacity loss look like it went away.
func (l *local) countQuarantined() (int, error) {
	infos, err := l.readAll()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, info := range infos {
		if info.quarantined() {
			n++
		}
	}
	return n, nil
}

func (l *local) deleteSandBoxInfo(sandBoxID string) error {
	l.metadataMu.Lock()
	defer l.metadataMu.Unlock()

	info, readErr := l.readSandBoxInfo(sandBoxID)
	if readErr != nil && readErr != utils.ErrorKeyNotFound && readErr != utils.ErrorBucketNotFound {
		return readErr
	}
	wasQuarantined := readErr == nil && info.quarantined()

	if err := l.db.Delete(bucketName, sandBoxID); err != nil && err != utils.ErrorKeyNotFound &&
		err != utils.ErrorBucketNotFound {
		return err
	}
	if wasQuarantined {
		quarantinedSandbox.Dec()
	}
	return nil
}

// readAll returns every recorded cleanup-pending sandbox.
//
// A record that fails to decode is skipped rather than failing the whole
// call: one corrupt entry must not stop every other sandbox from being
// cleaned up, and must not stop cubelet from starting (this is also called
// from the gauge-seeding path at init time).
func (l *local) readAll() (infos []*sandBoxInfo, _ error) {
	all, err := l.db.ReadAll(bucketName)
	if err != nil {
		return nil, err
	}

	for k, v := range all {
		bf := &sandBoxInfo{}
		if err := jsoniter.Unmarshal(v, bf); err != nil {
			CubeLog.Warnf("decode cleanup record %s: %v; skipping it", k, err)
			cleanupScheduler.WithValues(schedulerReadError).Inc()
			continue
		}
		infos = append(infos, bf)
	}

	return infos, nil
}
