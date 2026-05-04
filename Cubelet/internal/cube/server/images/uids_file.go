// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (c *CubeImageService) GenImageExtraAttributes(ctx context.Context, oldimg, i images.Image, snapshotter string) (fieldPaths []string, err error) {
	log := log.G(ctx).WithFields(CubeLog.Fields{
		"ref":         i.Name,
		"snapshotter": snapshotter,
	})

	if snapshotter == "" {

		snapshotter = c.defaultSnapshotter
	}

	var ns string
	ns, err = namespaces.NamespaceRequired(ctx)
	if err != nil {
		err = fmt.Errorf("get namespace: %w", err)
		return
	}

	var containerdImage containerd.Image
	containerdImage, err = c.client.GetImage(ctx, i.Name)
	if err != nil {
		return
	}

	if v := i.Labels[constants.LabelImageNoHostLayers]; v == constants.StringTrueValue {

	} else if _, ok := oldimg.Labels[constants.LabelImageHostLowerDirs]; !ok {
		if oldimg.Labels[constants.LabelImageHostLowerDirsPrefix] == "" {
			startTime := time.Now()
			defer func() {
				workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "image_prepare_lower_dirs", time.Since(startTime), time.Millisecond)
			}()
			diffIDs, err := containerdImage.RootFS(ctx)
			if err != nil {
				return nil, err
			}
			chainID := identity.ChainID(diffIDs).String()
			dirs, err := rootfs.SnapshotRefFs(ctx, c.client.SnapshotService(snapshotter), chainID)
			if err != nil {
				return nil, err
			}

			prefix := utils.MaxCommonPrefix(dirs)
			if len(prefix) > 1 {
				i.Labels[constants.LabelImageHostLowerDirsPrefix] = prefix
				fieldPaths = append(fieldPaths, "labels."+constants.LabelImageHostLowerDirsPrefix)

				dirs = utils.RemoveStringPrefix(dirs, prefix)
				s, _ := json.Marshal(dirs)
				i.Labels[constants.LabelImageLayerDirs] = string(s)
				fieldPaths = append(fieldPaths, "labels."+constants.LabelImageLayerDirs)
			} else {
				s, _ := json.Marshal(dirs)
				i.Labels[constants.LabelImageHostLowerDirs] = string(s)
				fieldPaths = append(fieldPaths, "labels."+constants.LabelImageHostLowerDirs)
			}
		}
	}

	_, uidsFileExists := i.Labels[constants.LabelImageUidFiles]
	if !uidsFileExists {
		startTime := time.Now()
		defer func() {
			workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "image_prepare_uids_time", time.Since(startTime), time.Millisecond)
		}()
		log.Debugf("try to generate uid files for image")

		uidFile := filepath.Join(c.uidDir, ns, i.Target.Digest.String())
		if localRoot, ok := i.Labels[constants.LabelImageHostLowerDirsPrefix]; ok && localRoot != "" {
			uidFile = filepath.Join(localRoot, "uids_file")
			info, statErr := os.Stat(uidFile)
			if statErr == nil && !info.IsDir() {
				// NOCC:Path Traversal()
				if removeErr := os.RemoveAll(uidFile); removeErr != nil {
					return nil, fmt.Errorf("remove invalid uid file path %s: %w", uidFile, removeErr)
				}
				statErr = os.ErrNotExist
			}
			if statErr != nil {
				if !os.IsNotExist(statErr) {
					return nil, fmt.Errorf("stat uid file path %s: %w", uidFile, statErr)
				}
				if err = CopyImageUidsFile(ctx, c.client, snapshotter, containerdImage, uidFile); err != nil {
					log.WithError(err).Errorf("failed to copy uids file: %v", err)
					return
				}
				info, statErr = os.Stat(uidFile)
				if statErr != nil {
					return nil, fmt.Errorf("uid file path %s missing after copy: %w", uidFile, statErr)
				}
				if !info.IsDir() {
					return nil, fmt.Errorf("uid file path %s is not a directory after copy", uidFile)
				}
			}
		} else {
			if err = CopyImageUidsFile(ctx, c.client, snapshotter, containerdImage, uidFile); err != nil {
				log.WithError(err).Errorf("failed to copy uids file: %v", err)
				return
			}
			info, statErr := os.Stat(uidFile)
			if statErr != nil {
				return nil, fmt.Errorf("uid file path %s missing after copy: %w", uidFile, statErr)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("uid file path %s is not a directory after copy", uidFile)
			}
		}

		i.Labels[constants.LabelImageUidFiles] = uidFile
		fieldPaths = append(fieldPaths, "labels."+constants.LabelImageUidFiles)
		log.Infof("generate uid files for image success")
	}

	return
}

func CopyImageUidsFile(ctx context.Context, client *containerd.Client, snapshotter string, i containerd.Image, target string) error {
	log := log.G(ctx).WithFields(CubeLog.Fields{
		"function":    "copyUidsFile",
		"target":      target,
		"snapshotter": snapshotter,
		"image":       i.Metadata().Name,
	})
	diffIDs, err := i.RootFS(ctx)
	if err != nil {
		return err
	}
	chainID := identity.ChainID(diffIDs).String()

	s := client.SnapshotService(snapshotter)
	viewKey := fmt.Sprintf("uids-view-%s-%s", chainID, utils.GenerateID())

	var mounts []mount.Mount
	mounts, err = s.View(ctx, viewKey, chainID)
	if err != nil {
		return err
	}
	defer func() {
		if err := s.Remove(ctx, viewKey); err != nil && !errdefs.IsNotFound(err) {
			log.WithError(err).Errorf("failed to remove snapshot %s: %v", viewKey, err)
		}
	}()

	err = mount.WithTempMount(ctx, mounts, func(root string) error {
		return CopyUidsFile(ctx, root, target)
	})
	if err != nil {
		log.WithError(err).Errorf("failed to copy uid files: %v", err)
		return err
	}

	return nil
}

func CopyUidsFile(ctx context.Context, src, dst string) error {
	var (
		err      error
		start    = time.Now()
		logEntry = log.G(ctx).WithFields(CubeLog.Fields{
			"src":    src,
			"dst":    dst,
			"method": "CopyUidsFile",
		})
	)

	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "image_prepare_uids", time.Since(start), time.Millisecond)
	}()

	rootfsCacheDir := filepath.Join(dst, "etc")
	if err = os.MkdirAll(rootfsCacheDir, 0755); err != nil {
		logEntry.Errorf("failed to create rootfs cache dir %s: %v", rootfsCacheDir, err)
		return err
	}
	uidSource := filepath.Join(src, "/etc/passwd")
	uidFile := filepath.Join(rootfsCacheDir, "passwd")
	if err := utils.SafeCopyFile(uidFile, uidSource); err != nil && !os.IsNotExist(err) {
		return err
	}

	gidSource := filepath.Join(src, "/etc/group")
	gidFile := filepath.Join(rootfsCacheDir, "group")
	if err := utils.SafeCopyFile(gidFile, gidSource); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
