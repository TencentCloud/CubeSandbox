// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"

	"github.com/containerd/cgroups/v3"
	"github.com/shopspring/decimal"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	v1 "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle/v1"
	v2 "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle/v2"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type CgPlugin struct {
	config       *Config
	overhead     *OverheadConfig
	poolV1Handle handle.Interface
	poolV2Handle handle.Interface
	pool         *cgPool
	db           *utils.CubeStore

	vmSnapshotSpecs VMSnapshotSpecsByProduct
	vmSpecLock      sync.RWMutex
}

type VMSnapshotSpecsByProduct map[string][]VmSnapshotSpec

var (
	l                              = &CgPlugin{}
	dbDir                          = "db"
	bucket                         = "sandbox"
	defaultPoolSize                = 50
	defaultPoolTriggerIntervalInMs = 1000
	defaultVmMemoryOverhead        = "42Mi"
	defaultMemoryCoefficient       = 64
	defaultVmCpuOverhead           = "0"
	defaultHostCpuOverhead         = "0"
	defaultHostMemoryOverhead      = "24Mi"
	defaultCubeMsgMemoryOverhead   = "16Mi"
	hostMetricsBaselineAttempts    = 3
	hostMetricsBaselineRetryDelay  = 10 * time.Millisecond

	cubeletCgroup = "/cube_sandbox/cubelet"

	registerBucket = multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     bucket,
			DbName:   "cgroup",
			Describe: "cgroup plugin db",
		},
	}
)

func (l *CgPlugin) init() error {
	l.poolV1Handle = getDefaultCgroupHandle(1)
	l.poolV2Handle = getDefaultCgroupHandle(2)
	if l.config.PoolSize <= 0 {
		l.config.PoolSize = defaultPoolSize
	}
	if l.config.PoolTriggerIntervalInMs <= 0 {
		l.config.PoolTriggerIntervalInMs = defaultPoolTriggerIntervalInMs
	}

	var err error
	l.overhead, err = parseOverheadConfig(l.config)
	if err != nil {
		return err
	}

	vmSnapshotSpecMap, err := loadVmSnapshotSpec(l.config.VmSnapshotSpecsConfig, l.overhead)
	if err != nil {
		CubeLog.Warnf("load vm snapshot spec failed: %w", err)
	} else {
		l.vmSnapshotSpecs = vmSnapshotSpecMap
	}

	err = l.initDb()
	if err != nil {
		return fmt.Errorf("init cgroup db: %w", err)
	}

	l.config.parseAndSetDynamicConfig(dynamConf.GetConfig())

	err = l.initPool()
	if err != nil {
		return fmt.Errorf("init cgroup pool: %w", err)
	}

	reparentVal := l.config.ShouldSetMemoryReparentFile()
	err = setupAllCgroupsMemoryReparentFile(context.Background(), reparentVal)
	if err != nil {
		return fmt.Errorf("cginit: failed to setup all cgroups memory.reparent_file to %v: %w", reparentVal, err)
	}

	dynamConf.AppendConfigWatcher(&cgPluginConfigWatcher{})
	if err = l.createAndPlaceSelfCgroup(cubeletCgroup); err != nil {
		return err
	}

	return nil
}

func getDefaultCgroupHandle(poolVersion int) handle.Interface {
	if cgroups.Mode() == cgroups.Unified {
		CubeLog.Infof("cgroup version v2")
		return v2.NewV2Handle(poolVersion)
	}
	CubeLog.Infof("cgroup version v1")
	return v1.NewV1Handle(poolVersion)
}

func parseOverheadConfig(c *Config) (*OverheadConfig, error) {
	if c.VmMemoryOverheadBase == "" {
		c.VmMemoryOverheadBase = defaultVmMemoryOverhead
	}
	if c.VmMemoryOverheadCoefficient == 0 {
		c.VmMemoryOverheadCoefficient = int64(defaultMemoryCoefficient)
	}
	if c.VmCpuOverhead == "" {
		c.VmCpuOverhead = defaultVmCpuOverhead
	}
	if c.HostCpuOverhead == "0" {
		c.VmCpuOverhead = defaultHostCpuOverhead
	}
	if c.HostMemoryOverheadBase == "" {
		c.HostMemoryOverheadBase = defaultHostMemoryOverhead
	}
	if c.CubeMsgMemoryOverhead == "" {
		c.CubeMsgMemoryOverhead = defaultCubeMsgMemoryOverhead
	}

	if c.SnapshotDiskDir == "" {
		c.SnapshotDiskDir = "/data/snapshot_pack/disks"
	}

	oc := &OverheadConfig{}
	var err error
	oc.VmMemoryBase, err = resource.ParseQuantity(c.VmMemoryOverheadBase)
	if err != nil {
		return nil, fmt.Errorf("parse VmMemoryBase %w", err)
	}
	oc.VmMemoryCoefficient = decimal.NewFromInt(c.VmMemoryOverheadCoefficient)
	oc.HostMemoryBase, err = resource.ParseQuantity(c.HostMemoryOverheadBase)
	if err != nil {
		return nil, fmt.Errorf("parse HostMemoryBase %w", err)
	}
	oc.CubeMsgMemory, err = resource.ParseQuantity(c.CubeMsgMemoryOverhead)
	if err != nil {
		return nil, fmt.Errorf("parse CubeMsgMemory %w", err)
	}

	oc.VmCpu, err = resource.ParseQuantity(c.VmCpuOverhead)
	if err != nil {
		return nil, fmt.Errorf("parse VmCpu %w", err)
	}
	oc.HostCpu, err = resource.ParseQuantity(c.HostCpuOverhead)
	if err != nil {
		return nil, fmt.Errorf("parse HostCpu %w", err)
	}
	oc.SnapshotDiskDir = c.SnapshotDiskDir
	return oc, nil
}

func (l *CgPlugin) createAndPlaceSelfCgroup(group string) error {
	if err := l.poolV1Handle.Create(context.Background(), group); err != nil {
		return fmt.Errorf("create cgroup %q: %w", group, err)
	}
	if err := l.poolV1Handle.AddProc(group, uint64(os.Getpid())); err != nil {
		return fmt.Errorf("add cubelet self to cgroup: %w", err)
	}
	return nil
}

func (l *CgPlugin) initDb() error {
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

func (l *CgPlugin) initPool() error {
	p := &cgPool{
		initialSize:  dynamConf.GetPoolSizeForInit(l.config.PoolSize),
		db:           l.db,
		poolV1Handle: l.poolV1Handle,
		poolV2Handle: l.poolV2Handle,
	}
	if err := p.init(); err != nil {
		return err
	}
	l.pool = p
	return nil
}

func (l *CgPlugin) ReloadVmSnapshotSpecs() error {
	vmSnapshotSpecList, err := loadVmSnapshotSpec(l.config.VmSnapshotSpecsConfig, l.overhead)
	if err != nil {
		return fmt.Errorf("load vm snapshot spec failed: %w", err)
	}
	l.vmSpecLock.Lock()
	defer l.vmSpecLock.Unlock()
	l.vmSnapshotSpecs = vmSnapshotSpecList
	return nil
}

func (l *CgPlugin) RegisterOperation(mux *http.ServeMux) error {
	mux.HandleFunc("/v1/reloadvmsnapshotspecs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if err := l.ReloadVmSnapshotSpecs(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return nil
}

func (l *CgPlugin) ID() string {
	return constants.CgroupID.ID()
}

func (l *CgPlugin) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log.G(ctx).Errorf("Init doing")
	defer log.G(ctx).Errorf("Init end")
	err := l.db.Close()
	if err != nil {
		return err
	}
	time.Sleep(time.Second)
	if err := os.RemoveAll(path.Clean(l.config.RootPath)); err != nil {
		return fmt.Errorf("%v  RemoveAll failed:%v", l.config.RootPath, err.Error())
	}

	if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("%v MkdirAll RootPath failed: %s", l.config.RootPath, err.Error())
	}

	err = l.poolV1Handle.Delete(ctx, handle.DefaultPathPoolV1)
	if err != nil {
		log.G(ctx).Warnf("delete old cgroup error:%s", err)
	}

	err = l.poolV2Handle.Delete(ctx, handle.DefaultPathPoolV2)
	if err != nil {
		log.G(ctx).Warnf("delete old cgroup error:%s", err)
	}
	err = l.initDb()
	if err != nil {
		log.G(ctx).Errorf("init cgroup db error:%s", err)
		return err
	}

	err = l.initPool()
	if err != nil {
		log.G(ctx).Errorf("init cgroup pool error:%s", err)
		return err
	}

	return nil
}

func (l *CgPlugin) Create(ctx context.Context, opts *workflow.CreateContext) (err error) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	realReq := opts.ReqInfo
	if opts.IsCreateSnapshot() {
		cgIDStr, err := l.db.Get(bucket, opts.GetSandboxID())
		if err == nil && string(cgIDStr) != "" {
			return ret.Err(errorcode.ErrorCode_PreConditionFailed, "already exists")
		}
	}
	var fullCgID *uint32
	usePoolV2 := false
	numa := opts.GetNumaNode()

	defer func() {
		if err != nil && fullCgID != nil {
			l.pool.Put(ctx, *fullCgID)
			log.G(ctx).Errorf("create error and delete cgroup %v", MakeCgroupPathByID(*fullCgID))
		}
	}()
	defer func() {
		retErr := utils.Recover()
		if retErr != nil {
			err = retErr
		}
	}()
	resourceQuantity, err := l.overhead.GetResourceWithOverhead(ctx, realReq, opts.VolumeInfo)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "%s", err)
	}

	l.vmSpecLock.RLock()
	vmSnapshotSpecs := l.vmSnapshotSpecs[opts.GetInstanceType()]
	vmSnapshotSpec, resourceQuantity := l.overhead.MatchVMSnapshotSpec(ctx, *resourceQuantity, vmSnapshotSpecs, opts.GetInstanceType())
	log.G(ctx).Infof("MatchVMSnapshotSpec result:%s", utils.InterfaceToString(vmSnapshotSpec))
	if vmSnapshotSpec.PreservedMemory > vmSnapshotSpec.SnapPreservedMemory {
		log.G(ctx).Errorf("MatchVMSnapshotSpec vmSnapshotSpec.PreservedMemory:%d>vmSnapshotSpec.SnapPreservedMemory:%d",
			vmSnapshotSpec.PreservedMemory, vmSnapshotSpec.SnapPreservedMemory)
	}
	l.vmSpecLock.RUnlock()

	fullCgID, err = l.pool.Get(ctx, opts.GetSandboxID(), usePoolV2, numa)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateCgroupFailed, "%s", err)
	}
	cgroupPath := MakeCgroupPathByID(*fullCgID)
	baseline, err := l.hostMetricsBaselineAtAssignment(ctx, cgroupPath)
	if err != nil {
		log.G(ctx).Warnf("capture host metrics baseline for cgroup assignment %s: %v", cgroupPath, err)
	}
	cgIDStr := strconv.Itoa(int(*fullCgID))
	err = l.db.Set(bucket, opts.GetSandboxID(), []byte(cgIDStr))
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateCgroupFailed, "failed save to db: %v", err)
	}

	opts.CgroupInfo = &Info{
		CgroupID:                               cgroupPath,
		ResourceQuantity:                       *resourceQuantity,
		VmSnapshotSpec:                         vmSnapshotSpec,
		UsePoolV2:                              usePoolV2,
		HostMetricsBaseline:                    baseline,
		HostMetricsBaselineMissingAtAssignment: baseline == nil,
	}

	return nil
}

func (l *CgPlugin) setupMemoryReparentFile(ctx context.Context, fullCgID uint32, set bool) error {

	var err error

	cgPath := MakeCgroupPathByID(fullCgID)
	filePath := fmt.Sprintf("/sys/fs/cgroup/%s/memory.reparent_file",
		cgPath)

	if _, err = os.Stat(filePath); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	value := "0"
	if set {
		value = "1"
	}

	defer func() {
		if err != nil {
			log.G(ctx).Warnf("setupMemoryReparentFile(%s) to %s failed: %v", cgPath, value, err)
		}
	}()

	if err = os.WriteFile(filePath, []byte(value), 0644); err != nil {
		err = fmt.Errorf("failed to write %s: %v", filePath, err)
		return nil
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		err = fmt.Errorf("failed to read %s: %v", filePath, err)
		return nil
	}
	content = bytes.TrimSpace(content)
	if string(content) != value {
		err = fmt.Errorf("failed to set memory.reparent_file to %s, got %s", value, content)
		return nil
	}

	return nil

}

func (l *CgPlugin) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	cgIDStr, err := l.db.Get(bucket, opts.SandboxID)

	if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
		return nil
	}

	fullcgID, err := strconv.Atoi(string(cgIDStr))
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_DestroyCgroupFailed, "invalid cgroup id %s", string(cgIDStr))
	}

	l.pool.Put(ctx, uint32(fullcgID))
	err = l.db.Delete(bucket, opts.SandboxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
		return ret.Errorf(errorcode.ErrorCode_DestroyCgroupFailed, "failed delete from db: %v", err)
	}
	return nil
}

func (l *CgPlugin) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {

		errs := l.pool.Tidy()
		if len(errs) > 0 {
			return fmt.Errorf("multiple errors occurred during tidy: %v", errs)
		}
		return nil
	}
	sandBoxID := opts.SandboxID
	if err := l.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: sandBoxID,
		},
	}); err != nil {
		log.G(ctx).Errorf("CleanUp fail:%v", err)
		return err
	}
	return nil
}

type CgroupMetrics struct {
	QuotaCpuUsage   int   `json:"quota_cpu_usage,omitempty"`
	QuotaMemMbUsage int64 `json:"quota_mem_mb_usage,omitempty"`
	MvmNum          int   `json:"mvm_num,omitempty"`
}

func (l *CgPlugin) CollectMetric(ctx context.Context) *CgroupMetrics {
	CubeLog.WithContext(context.Background()).Infof("start collect CgroupMetrics")
	defer utils.Recover()
	var quotaMCpuUsage int
	var quotaMemUsage int64
	metrics := &CgroupMetrics{}

	inuseCgroup := l.pool.All()
	for _, cgID := range inuseCgroup {
		quotaMCpuUsage += l.poolV1Handle.GetAllocatedCpuNum(MakeCgroupPathByID(cgID))
		quotaMemUsage += l.poolV1Handle.GetAllocatedMem(MakeCgroupPathByID(cgID))
	}

	quotaMemMbUsage := quotaMemUsage / 1024 / 1024
	metrics.MvmNum = len(inuseCgroup)
	metrics.QuotaCpuUsage = quotaMCpuUsage
	metrics.QuotaMemMbUsage = quotaMemMbUsage
	log.G(ctx).Infof("collect cgroup cpu:%v,mem:%v", quotaMCpuUsage, quotaMemUsage)
	return metrics
}

// UsageSnapshot reads the host cgroup usage for one persisted CubeBox group.
// The same pool handle that owns the group path also owns its Linux ABI reader.
func (l *CgPlugin) UsageSnapshot(ctx context.Context, group string) (handle.UsageSnapshot, error) {
	group = path.Clean(group)
	if group == "." || group == "/" {
		return handle.UsageSnapshot{}, fmt.Errorf("host cgroup path is required")
	}
	switch {
	case group == handle.DefaultPathPoolV1 || strings.HasPrefix(group, handle.DefaultPathPoolV1+string(os.PathSeparator)):
		return l.poolV1Handle.UsageSnapshot(ctx, group)
	case group == handle.DefaultPathPoolV2 || strings.HasPrefix(group, handle.DefaultPathPoolV2+string(os.PathSeparator)):
		return l.poolV2Handle.UsageSnapshot(ctx, group)
	default:
		return handle.UsageSnapshot{}, fmt.Errorf("host cgroup path %q is outside CubeSandbox pools", group)
	}
}

func (l *CgPlugin) hostMetricsBaselineAtAssignment(ctx context.Context, group string) (*cubeboxstore.HostMetricsBaseline, error) {
	var usage handle.UsageSnapshot
	var err error
	for attempt := 1; attempt <= hostMetricsBaselineAttempts; attempt++ {
		usage, err = l.UsageSnapshot(ctx, group)
		if err == nil {
			break
		}
		if attempt == hostMetricsBaselineAttempts {
			return nil, err
		}
		timer := time.NewTimer(hostMetricsBaselineRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return &cubeboxstore.HostMetricsBaseline{
		CGroupPath:               group,
		CPUUsageTotalNS:          usage.CPUUsageTotalNS,
		CPUUserTotalNS:           usage.CPUUserTotalNS,
		CPUSystemTotalNS:         usage.CPUSystemTotalNS,
		CPUThrottledTotalNS:      usage.CPUThrottledTotalNS,
		CPUPeriodsTotal:          usage.CPUPeriodsTotal,
		CPUThrottledPeriodsTotal: usage.CPUThrottledPeriodsTotal,
		MemoryFailuresTotal:      usage.MemoryFailuresTotal,
	}, nil
}

func ceilMemQuota(q resource.Quantity) resource.Quantity {
	f := math.Ceil(float64(q.Value())/1024/1024) * 1024 * 1024
	return *resource.NewQuantity(int64(f), resource.BinarySI)
}

type Info struct {
	CgroupID                               string
	ResourceQuantity                       cubeboxstore.ResourceWithOverHead
	VmSnapshotSpec                         CubeVMMResource
	UsePoolV2                              bool
	HostMetricsBaseline                    *cubeboxstore.HostMetricsBaseline
	HostMetricsBaselineMissingAtAssignment bool
}
