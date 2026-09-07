// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package virtiofs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
)

type ShareDirMapping struct {
	SharePath string
	MountPath string
}

const virtioFsSharePath = "/data/cubelet/"

type VirtiofsConfig struct {
	ID                    string                `json:"id,omitempty"`
	PropagationMountName  string                `json:"propagation_mount_name,omitempty"`
	RateLimiter           disk.RateLimiter      `json:"rate_limiter_config"`
	VirtioBackendFsConfig VirtioBackendFsConfig `json:"backendfs_config"`
}

type VirtioBackendFsConfig struct {
	SharedDir   string   `json:"shared_dir"`
	AllowedDirs []string `json:"allowed_dirs"`
	ReadOnly    bool     `json:"read_only"`
	Cache       int      `json:"cache"`
	// RemapFilter makes restore-time allowed_dirs authoritative for serialized
	// filter roots. It is enabled only when restoring guest mount state.
	RemapFilter bool `json:"remap_filter,omitempty"`
}

type ErofsImagePath struct {
	Path string `json:"path"`

	LowerDir []string `json:"lower_dir"`
}
type CubeRootfsInfo struct {
	Overlay    *CubeRootfsOverlayInfo `json:"overlay_info,omitempty"`
	Mounts     []CubeRootfsMount      `json:"mounts,omitempty"`
	PmemFile   string                 `json:"pmem_file,omitempty"`
	ErofsImage *ErofsImagePath        `json:"ero_image,omitempty"`
}

type CubeRootfsOverlayInfo struct {
	VirtiofsLowerDir []string `json:"virtiofs_lower_dir,omitempty"`
	HostLowerDir     []string `json:"-"`
}

type CubeRootfsMount struct {
	HostSource     string   `json:"-"`
	VirtiofsID     string   `json:"virtiofs_id"`
	VirtiofsSource string   `json:"virtiofs_source"`
	ContainerDest  string   `json:"container_dest"`
	Type           string   `json:"type"`
	Options        []string `json:"options"`
}

type VirtioMounts struct {
	HostSource     string `json:"-"`
	VirtiofsSource string `json:"source"`
	Destination    string `json:"dest"`
}

type PropagationContainerDirs struct {
	Name         string `json:"name"`
	ContainerDir string `json:"container_dir"`
}

var emptyShareDirPath string

func Init(dataDir string) {
	emptyShareDirPath = filepath.Join(dataDir, "empty_share")
	_ = os.MkdirAll(emptyShareDirPath, os.ModeDir|0755)
}

func CheckVmRelativePath(path string) bool {
	if path == "/" {
		return true
	}
	up := ".." + string(os.PathSeparator)
	rel, err := filepath.Rel(virtioFsSharePath, path)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(rel, up) && rel != ".." {
		return true
	}
	return false
}

func GenVirtiofsConfig(shared []string) (*VirtiofsConfig, error) {
	virtiofsConfig := &VirtiofsConfig{
		VirtioBackendFsConfig: VirtioBackendFsConfig{
			SharedDir: virtioFsSharePath,
			ReadOnly:  true,
			Cache:     constants.VirtiofsCacheNever,
		},
	}

	sharedNames := sets.NewString()
	sharedDirSet := sets.NewString()

	var shareDirs []string
	for _, dir := range shared {
		if sharedDirSet.Has(dir) {
			continue
		}
		sharedDirSet.Insert(dir)
		err := addAndCheckSharePath(dir, &sharedNames)
		if err != nil {
			if strings.Contains(err.Error(), "duplicated") {
				continue
			}
			return nil, err
		}
		shareDirs = append(shareDirs, dir)
	}
	virtiofsConfig.VirtioBackendFsConfig.AllowedDirs = shareDirs

	return virtiofsConfig, nil
}

func GenEmptyVirtiofsConfig(readOnly bool, cache int) (*VirtiofsConfig, error) {
	virtiofsConfig, _ := GenVirtiofsConfig([]string{})
	virtiofsConfig.VirtioBackendFsConfig.SharedDir = emptyShareDirPath
	virtiofsConfig.VirtioBackendFsConfig.AllowedDirs = nil
	virtiofsConfig.VirtioBackendFsConfig.Cache = cache
	virtiofsConfig.VirtioBackendFsConfig.ReadOnly = readOnly
	if readOnly {
		virtiofsConfig.PropagationMountName = constants.PropagationVirtioRo
		virtiofsConfig.ID = constants.PropagationVirtioRo
	} else {
		virtiofsConfig.PropagationMountName = constants.PropagationVirtioRw
		virtiofsConfig.ID = constants.PropagationVirtioRw
	}
	return virtiofsConfig, nil
}

func addAndCheckSharePath(sharePath string, sharedNames *sets.String) error {
	sharedName := filepath.Base(sharePath)
	if sharedNames.Has(sharedName) {
		return fmt.Errorf("share path %s is duplicated", sharePath)
	}
	sharedNames.Insert(sharedName)
	if !CheckVmRelativePath(sharePath) {
		return fmt.Errorf("not a valid share path: %s", sharePath)
	}
	return nil
}

func GenMountConfig(mounts []specs.Mount) ([]CubeRootfsMount, error) {
	var cubeMounts []CubeRootfsMount
	for _, m := range mounts {
		if !CheckVmRelativePath(m.Source) {
			return nil, fmt.Errorf("not a valid share path: %s", m.Source)
		}
		cubeMounts = append(cubeMounts, CubeRootfsMount{
			HostSource:     m.Source,
			VirtiofsSource: filepath.Base(m.Source),
			ContainerDest:  m.Destination,
			Type:           m.Type,
			Options:        m.Options,
		})
	}

	return cubeMounts, nil
}

func GenOverlayMountConfig(ovlShare []ShareDirMapping) *CubeRootfsOverlayInfo {
	overlayInfo := &CubeRootfsOverlayInfo{}
	for _, dir := range ovlShare {
		overlayInfo.VirtiofsLowerDir = append(overlayInfo.VirtiofsLowerDir, dir.MountPath)
		overlayInfo.HostLowerDir = append(overlayInfo.HostLowerDir, dir.SharePath)
	}
	return overlayInfo
}

func (i *CubeRootfsInfo) ShareDirs() []string {
	var shareDir []string
	for _, m := range i.Mounts {
		if m.HostSource != "" {
			shareDir = append(shareDir, m.HostSource)
		}
	}
	if i.Overlay != nil && len(i.Overlay.HostLowerDir) > 0 {
		shareDir = append(shareDir, i.Overlay.HostLowerDir...)
	}
	return shareDir
}

func GenPropagationVirtioDirs() string {

	return fmt.Sprintf(`[{"name":"%s"},{"name":"%s"}]`, constants.PropagationVirtioRo, constants.PropagationVirtioRw)
}

func GenPropagationContainerDirs() string {
	const s = `[{"name":"%s","container_dir":"%s"},{"name":"%s","container_dir":"%s"}]`
	return fmt.Sprintf(s, constants.PropagationVirtioRo, constants.PropagationContainerDirRo,
		constants.PropagationVirtioRw, constants.PropagationContainerDirRw)
}

func GenPropagationContainerUmounts() string {
	const s = `[{"name":"%s","container_dir":"%s"},{"name":"%s","container_dir":"%s"}]`
	return fmt.Sprintf(s, constants.PropagationVirtioRo, constants.PropagationContainerDirRo,
		constants.PropagationVirtioRw, constants.PropagationContainerDirRw)
}
