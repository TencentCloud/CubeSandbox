// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimlog

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/fifo"
	"encoding/json"
	"encoding/json"

	"github.com/moby/sys/mountinfo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/taskio"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/sys/unix"
)

type local struct {
	sync.Map
	config     *Config
	shimLogger *CubeLog.Logger
}

func (l *local) ID() string {
	return constants.ShimLogID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	CubeLog.WithContext(ctx).Errorf("Init doing")
	defer CubeLog.WithContext(ctx).Errorf("Init end")

	l.Range(func(key, value any) bool {
		if value != nil {
			cancelFun, ok := value.(func())
			if ok {
				cancelFun()
			}
		}
		l.Delete(key)
		return true
	})
	time.Sleep(time.Second)
	_ = mount.UnmountAll(l.config.RootPath, 0)
	if err := os.RemoveAll(path.Clean(l.config.RootPath)); err != nil {
		CubeLog.WithContext(ctx).Infof("init fail,RemoveAll err:%v", err)
		return err
	}

	if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init RootPath dir failed, %s", err.Error())
	}
	size := l.config.TmpfsSize
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

	return nil
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	select {

	case <-ctx.Done():
		return nil
	default:
	}
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	// 1.2.1 路径穿越防护：校验 SandboxID 不含路径穿越字符
	sandboxDir, err := utils.SafeJoinPath(filepath.Join(l.config.RootPath, namespace), opts.SandboxID)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid sandboxID: %v", err)
	}
	if err := os.MkdirAll(sandboxDir, os.ModeDir|0755); err != nil {
		return ret.Err(errorcode.ErrorCode_CreateShimlogFailed, err.Error())
	}

	shimReqLogPath := filepath.Join(sandboxDir, l.config.ShimReqLogName)
	shimStatLogPath := filepath.Join(sandboxDir, l.config.ShimStatLogName)
	log.G(ctx).Debugf("log path:%s,%s", namespace, opts.SandboxID)

	mode := unix.O_RDONLY | unix.O_CREAT | unix.O_NONBLOCK
	fReq, err := fifo.OpenFifo(context.Background(), shimReqLogPath, mode, 0600)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateShimlogFailed, "create shim req log pipe: %v", err)
	}

	fStat, err := fifo.OpenFifo(context.Background(), shimStatLogPath, mode, 0600)
	if err != nil {
		fReq.Close()
		return ret.Errorf(errorcode.ErrorCode_CreateShimlogFailed, "create shim stat log pipe: %v", err)
	}

	cancel := func() {

		fReq.Close()
		fStat.Close()
	}
	l.LoadOrStore(opts.SandboxID, cancel)
	_ = l.shimReqlog(ctx, opts.SandboxID, fReq)
	_ = l.shimStatlog(ctx, opts.SandboxID, fStat)
	return nil
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	// 1.2.1 路径穿越防护：校验 SandboxID 不含路径穿越字符
	sandboxDir, err := utils.SafeJoinPath(filepath.Join(l.config.RootPath, namespace), opts.SandboxID)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid sandboxID: %v", err)
	}
	if ok, _ := utils.DenExist(sandboxDir); ok {
		if cancelFunc, ok := l.Load(opts.SandboxID); ok && cancelFunc != nil {

			cancelFunc.(func())()
		}

		if err := os.RemoveAll(sandboxDir); err != nil {
			CubeLog.WithContext(ctx).Infof("RemoveAll err:%v", err)
			return ret.Errorf(errorcode.ErrorCode_DestroyShimlogFailed, "create shim stat log pipe: %v", err)
		}
	}
	return nil
}

func (l *local) shimReqlog(ctx context.Context, sandBoxID string, f io.ReadCloser) error {
	namespace, _ := namespaces.NamespaceRequired(ctx)
	ctx = context.WithValue(ctx, CubeLog.KeyInstanceId, sandBoxID)
	ctx = context.WithValue(ctx, CubeLog.KeyNamespace, namespace)
	shimReqLogPath := filepath.Join(l.config.RootPath, namespace, sandBoxID, l.config.ShimReqLogName)

	logEntry := l.shimLogger.WithContext(ctx)
	fields := CubeLog.Fields{"Module": "Shim"}
	shimLog := logEntry.WithFields(fields)

	recov.GoWithRecover(
		func() {
			defer f.Close()
			defer l.Delete(sandBoxID)
			logEntry.Debugf("%s begin doing", shimReqLogPath)
			buf := bufferPool.Get().([]byte)
			defer bufferPool.Put(buf)
			for {
				n, err := f.Read(buf)
				if err == nil {

					shimLog.Errorf(string(buf[:n]))
					continue
				}
				if err == io.EOF {
					logEntry.Debugf("read EOF from %s, read logs done", shimReqLogPath)
				} else {
					logEntry.Errorf("read %s failed: %+v", shimReqLogPath, err)
				}
				break
			}
			logEntry.Debugf("end copy:%s", shimReqLogPath)
		})
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	CubeLog.WithContext(ctx).Debugf("CleanUp doing")
	if opts == nil {

		return nil
	}
	sandBoxID := opts.SandboxID
	if err := l.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: sandBoxID,
		},
	}); err != nil {

		CubeLog.WithContext(ctx).WithFields(CubeLog.Fields{
			"plugin": "CgPlugin",
		}).Errorf("CleanUp fail:%v", err)
		return err
	}
	return nil
}

func (l *local) shimStatlog(ctx context.Context, sandBoxID string, f io.ReadCloser) error {
	namespace, _ := namespaces.NamespaceRequired(ctx)
	ShimStatLogPath := filepath.Join(l.config.RootPath, namespace, sandBoxID, l.config.ShimStatLogName)
	logEntry := l.shimLogger.WithContext(ctx)
	logEntry.Debugf("ShimStatLogPath:%s", ShimStatLogPath)

	recov.GoWithRecover(
		func() {
			defer f.Close()
			defer l.Delete(sandBoxID)
			logEntry.Infof("%s begin doing", ShimStatLogPath)
			var logBuf taskio.LogBuffer
			buf := bufferPool.Get().([]byte)
			defer bufferPool.Put(buf)
			for {
				n, err := f.Read(buf)
				if err == nil {
					lines := logBuf.Write(buf[:n])
					for _, line := range lines {
						trace := &CubeLog.RequestTrace{}
						err := json.Unmarshal(line, trace)
						if err != nil {
							logEntry.Warnf("json.Unmarshal ShimStatLog error: %+v", err)
							continue
						}

						trace.CallerIP = CubeLog.LocalIP
						trace.InstanceID = sandBoxID
						trace.Namespace = namespace
						CubeLog.Trace(trace)
					}
					continue
				}

				if err == io.EOF {
					logEntry.Debugf("read EOF from %s, consume logs done", ShimStatLogPath)
				} else {
					logEntry.Infof("read %s failed: %+v", ShimStatLogPath, err)
				}
				break
			}

			logEntry.Debugf("end copy:%s", ShimStatLogPath)
		})
	return nil
}

func Reload(ctx context.Context, id string) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	shimLogPath := filepath.Join(l.config.RootPath, ns, id)
	CubeLog.WithContext(ctx).Debugf("reload %s log path:%s,%s", id, l.config.ShimReqLogName,
		l.config.ShimStatLogName)
	shimReqLogPath := filepath.Join(shimLogPath, l.config.ShimReqLogName)
	shimStatLogPath := filepath.Join(shimLogPath, l.config.ShimStatLogName)
	mode := unix.O_RDONLY | unix.O_NONBLOCK
	fReq, err := fifo.OpenFifo(context.Background(), shimReqLogPath, mode, 0600)
	if err != nil {
		CubeLog.WithContext(ctx).Errorf("create shim req log pipe: %v", err)
		return err
	}

	fStat, err := fifo.OpenFifo(context.Background(), shimStatLogPath, mode, 0600)
	if err != nil {
		fReq.Close()
		CubeLog.WithContext(ctx).Errorf("create shim stat log pipe: %v", err)
		return err
	}

	cancel := func() {

		fReq.Close()
		fStat.Close()
	}
	l.LoadOrStore(id, cancel)

	_ = l.shimReqlog(ctx, id, fReq)
	_ = l.shimStatlog(ctx, id, fStat)
	return nil
}

func (l *local) reload(ctx context.Context) error {

	nsDirs, err := os.ReadDir(l.config.RootPath)
	if err != nil {
		return err
	}
	for _, nsd := range nsDirs {
		if !nsd.IsDir() {
			continue
		}
		ns := nsd.Name()

		if len(ns) > 0 && ns[0] == '.' {
			continue
		}
		CubeLog.WithContext(ctx).Debugf("loading shimlog in namespace %s", ns)
		shimDirs, err := os.ReadDir(filepath.Join(l.config.RootPath, ns))
		if err != nil {
			continue
		}

		for _, sd := range shimDirs {
			if !sd.IsDir() {
				continue
			}
			id := sd.Name()

			if len(id) > 0 && id[0] == '.' {
				continue
			}
			shimLogPath := filepath.Join(l.config.RootPath, ns, id)

			taskExist, _ := utils.DenExist(filepath.Join(l.config.TaskRootPath, ns, id))
			if !taskExist {
				if err := os.RemoveAll(shimLogPath); err != nil {
					CubeLog.WithContext(ctx).Errorf("reload remove:%v", shimLogPath)
				}
			}
		}
	}
	return nil
}
