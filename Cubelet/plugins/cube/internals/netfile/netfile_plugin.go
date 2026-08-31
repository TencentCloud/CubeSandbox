// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	localnetfile "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:     constants.InternalPlugin,
		ID:       constants.NetFile.ID(),
		Config:   defaultConfig(),
		Requires: []plugin.Type{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l := &netFilePlugin{}
			l.netFilePluginConfig = ic.Config.(*netFilePluginConfig)
			if l.RootPath == "" {

				l.RootPath = ic.Properties[plugins.PropertyStateDir]
			}
			err := localnetfile.Init(l.netFilePluginConfig.OldNetFilePath)
			if err != nil {
				return nil, err
			}
			log.G(ic.Context).Infof("init netfile plugin success, rootPath: %s", l.RootPath)
			return l, nil
		},
	})
}

type netFilePluginConfig struct {
	OldNetFilePath string `yaml:"old_net_file_path,omitempty" toml:"old_net_file_path,omitempty"`
	RootPath       string `yaml:"root_path,omitempty" toml:"root_path,omitempty"`
}

func defaultConfig() *netFilePluginConfig {
	return &netFilePluginConfig{
		OldNetFilePath: "/data/cubelet/root/io.cubelet.internal.v1.cubebox/netfile",
	}
}

type netFilePlugin struct {
	*netFilePluginConfig
}

func (l *netFilePlugin) ID() string {
	return constants.NetFile.ID()
}

func (l *netFilePlugin) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log := log.G(ctx).WithFields(CubeLog.Fields{
		"plugin": l.ID(),
	})
	log.Errorf("Init doing")
	defer log.Errorf("Init end")
	if err := os.RemoveAll(path.Clean(l.RootPath)); err != nil {
		log.Infof("init fail,RemoveAll err:%v", err)
		return err
	}

	if err := os.MkdirAll(l.RootPath, os.ModeDir|0755); err != nil {
		return fmt.Errorf("init RootPath dir failed, %s", err.Error())
	}

	return nil
}

func (l *netFilePlugin) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	log.G(ctx).Debug("Create doing")
	var (
		sandboxID = opts.SandboxID
	)

	cnfs := &localnetfile.CubeboxNetfile{
		Hostname: localnetfile.TrimHostName(sandboxID),
	}
	err := cnfs.CreateNetfiles(opts.ReqInfo)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}

	if !config.GetCommon().DisableHostNetfile {
		cnfs.RootPath = path.Join(l.RootPath, sandboxID)

		err = cnfs.WriteToHost()
		if err != nil {
			return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
		}
		if log.IsDebug() {
			log.G(ctx).WithFields(CubeLog.Fields{
				"netfile": utils.InterfaceToString(cnfs),
			}).Debugf("create netfile at %s", cnfs.RootPath)
		}
	}
	opts.NetFile = cnfs

	return nil
}

func (l *netFilePlugin) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return nil
	}
	return l.CleanUp(ctx, &workflow.CleanContext{
		BaseWorkflowInfo: opts.BaseWorkflowInfo,
	})
}

func (l *netFilePlugin) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}

	if err := cleanNetfileDir(ctx, path.Clean(path.Join(l.RootPath, opts.SandboxID))); err != nil {
		return err
	}
	if err := cleanNetfileDir(ctx, path.Clean(path.Join(l.OldNetFilePath, opts.SandboxID))); err != nil {
		return err
	}

	return nil
}

func cleanNetfileDir(ctx context.Context, dir string) error {
	exist, _ := utils.DenExist(dir)
	if exist {
		log.G(ctx).Errorf("clean netfile dir %s", dir)
		err := os.RemoveAll(dir) // NOCC:Path Traversal()
		if err != nil {
			return fmt.Errorf("remove netfile dir %s failed, %s", dir, err)
		}
	}
	return nil
}

var _ workflow.Flow = &netFilePlugin{}
