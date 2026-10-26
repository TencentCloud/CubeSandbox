// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package rootfs

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/hashicorp/go-multierror"
	imageSpec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

var rootfsBasePath string

func Init(dataDir string) {
	rootfsBasePath = path.Join(dataDir, "rootfs")
	_ = os.MkdirAll(rootfsBasePath, os.ModeDir|0755)
}

func GenRootfsOpt(
	ctx context.Context,
	c *cubebox.ContainerConfig,
	imgCfg *imagestore.Image,
) ([]oci.SpecOpts, error) {
	var opts []oci.SpecOpts
	if !constants.IsCubeRuntime(ctx) {
		return opts, nil
	}
	if len(imgCfg.References) == 0 {
		return opts, fmt.Errorf("rootfs lowerDirs is empty")
	}

	opts = append(opts, withRootFsCwd(&imgCfg.ImageSpec.Config))
	if c.GetSecurityContext().GetReadonlyRootfs() {
		opts = append(opts, oci.WithRootFSReadonly())
	}

	rootpath := imgCfg.UidFiles
	if rootpath == "" {
		return opts, fmt.Errorf("image %s rootfs uidFiles is empty", imgCfg.ID)
	}
	opts = append(opts, oci.WithRootFSPath(rootpath))

	return opts, nil
}

func GenImageSharedDirs(hostLayers []string) ([]virtiofs.ShareDirMapping, error) {
	return hostToShareDirs(hostLayers)
}

func hostToShareDirs(hostDirs []string) ([]virtiofs.ShareDirMapping, error) {
	if len(hostDirs) == 0 {
		return nil, fmt.Errorf("rootfs lowerDirs is empty")
	}
	var ovlShareDir []virtiofs.ShareDirMapping
	for _, dir := range hostDirs {
		ovl, err := HostToShareDir(dir)
		if err != nil {
			return nil, err
		}
		ovlShareDir = append(ovlShareDir, *ovl)
	}
	return ovlShareDir, nil
}

func HostToShareDir(dir string) (*virtiofs.ShareDirMapping, error) {
	var (
		sharePath string
		mountPath string
	)

	if !virtiofs.CheckVmRelativePath(dir) {
		return nil, fmt.Errorf("rootfs not a valid share dir:%v", dir)
	}
	if strings.HasSuffix(dir, "/fs") {

		sharePath = strings.TrimSuffix(dir, "/fs")
		mountPath = filepath.Join(filepath.Base(sharePath), "fs")
	} else {

		sharePath = dir
		mountPath = filepath.Base(sharePath)
	}
	return &virtiofs.ShareDirMapping{

		SharePath: sharePath,

		MountPath: mountPath,
	}, nil
}

func CleanRootfs(ctx context.Context, containerID string) error {
	cntrBaseDir, err := utils.SafeJoinPath(rootfsBasePath, containerID)
	if err != nil {
		return fmt.Errorf("CleanRootfs: %w", err)
	}
	rootfsMp := filepath.Join(cntrBaseDir, "rootfs")
	rootfsFs := filepath.Join(cntrBaseDir, "fs")
	rootfsWork := filepath.Join(cntrBaseDir, "work")

	rootfsDirs := []string{rootfsMp, rootfsFs, rootfsWork, cntrBaseDir}

	var (
		result *multierror.Error
	)
	if er := mount.UnmountAll(rootfsMp, 0); er != nil && er != syscall.ENOENT {
		result = multierror.Append(result, fmt.Errorf("umount [%s] fail: %w", rootfsMp, er))
	}

	for _, d := range rootfsDirs {
		if err := os.RemoveAll(path.Clean(d)); err != nil {
			result = multierror.Append(result, fmt.Errorf("RemoveAll [%s] fail: %w", d, err))
		}
	}
	if result.ErrorOrNil() != nil {
		log.G(ctx).Errorf("cleanRootfs fail:%v", result.Error())
	}
	return result.ErrorOrNil()
}

func withRootFsCwd(config *imageSpec.ImageConfig) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client,
		c *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}

		s.Process.Cwd = config.WorkingDir
		if s.Process.Cwd == "" {
			s.Process.Cwd = "/"
		}
		return nil
	}
}

func CreateRootfs(containerID string) (string, error) {
	p, err := utils.SafeJoinPath(rootfsBasePath, containerID)
	if err != nil {
		return "", fmt.Errorf("CreateRootfs: %w", err)
	}
	err = os.MkdirAll(p, os.ModeDir|0755)
	if err != nil {
		return "", fmt.Errorf("mkdir rootfs path [%s] fail:%v", p, err)
	}
	return p, nil
}
