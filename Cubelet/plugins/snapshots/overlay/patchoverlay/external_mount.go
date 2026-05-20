// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package patchoverlay

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	refDirName = "refs"
)

func WithCubeUseRefPath(config *SnapshotterConfig) error {
	config.useCubeRefPath = true
	return nil
}

func checkFileNotExist(path string) error {
	if path == "" {
		return nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return fmt.Errorf("ref path %s already exists: %w", path, errdefs.ErrAlreadyExists)
	}
	if os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("failed to stat ref path %s: %w", path, err)
}

func (o *snapshotter) makeCubeRefPathDir(ctx context.Context, info snapshots.Info) (string, error) {
	if o.useCubeRefPath {
		refPath, err := o.genCubeRefPath(ctx, info)
		if err != nil || refPath == "" {
			return "", err
		}

		err = checkFileNotExist(refPath)
		if err != nil {
			return refPath, err
		}
		err = os.MkdirAll(filepath.Dir(refPath), 0700)
		if err != nil {
			return "", fmt.Errorf("failed to create ref path %s: %w", refPath, err)
		}
		return refPath, nil
	}
	return "", nil
}

func (o *snapshotter) genCubeRefPath(ctx context.Context, info snapshots.Info) (string, error) {
	if o.useCubeRefPath {
		if ref, ok := info.Labels[constants.AnnotationSnapshotRef]; ok {
			var ns string
			sname, err := parseSnapshotName(info.Name)
			if err == nil {
				ns = sname.namespace
			} else {
				ns, err = namespaces.NamespaceRequired(ctx)
				if err != nil {
					return "", fmt.Errorf("failed to get namespace when new cube ref path: %w", err)
				}
			}
			cutref, _ := strings.CutPrefix(ref, constants.PrefixSha256)
			return filepath.Join(o.root, refDirName, ns, cutref), nil
		}
	}
	return "", nil
}

func (o *snapshotter) getValidCubeUpperPath(ctx context.Context, id string, info snapshots.Info) string {
	if !o.useCubeRefPath {
		return o.upperPath(id)
	}
	stepLogger := log.G(ctx).WithFields(CubeLog.Fields{
		"step": "getCubeUpperPath",
		"info": info.Name,
	})

	if ref, ok := info.Labels[constants.AnnotationSnapshotterExternalPath]; ok {
		return ref
	}

	var err error
	refpath, ok := info.Labels[constants.AnnotationSnapshotRefDir]
	if ok {
		return path.Join(refpath, "fs")
	}

	refpath, err = o.genCubeRefPath(ctx, info)
	if err != nil {
		stepLogger.WithError(err).Debug("failed to get cube ref path when get cube upper path")
		return o.upperPath(id)
	}
	if refpath == "" {
		return o.upperPath(id)
	}

	ref := filepath.Join(refpath, "fs")
	_, err = os.Stat(ref)
	if err != nil {
		stepLogger.WithError(err).Debug("failed to stat cube ref path when get cube upper path")
		return o.upperPath(id)
	}
	info.Labels[constants.AnnotationSnapshotRefDir] = refpath
	storage.UpdateInfo(ctx, info, "labels."+constants.AnnotationSnapshotRefDir)
	return ref
}

func (o *snapshotter) getCubeWorkPath(ctx context.Context, id string, info snapshots.Info) string {
	if !o.useCubeRefPath {
		return o.workPath(id)
	}

	var err error
	refpath, ok := info.Labels[constants.AnnotationSnapshotRefDir]
	if ok {
		return path.Join(refpath, "work")
	}
	refpath, err = o.genCubeRefPath(ctx, info)
	if err != nil {
		return o.workPath(id)
	}
	if refpath == "" {
		return o.workPath(id)
	}

	ref := filepath.Join(refpath, "work")
	_, err = os.Stat(ref)
	if err != nil {
		return o.workPath(id)
	}

	info.Labels[constants.AnnotationSnapshotRefDir] = refpath
	storage.UpdateInfo(ctx, info, "labels."+constants.AnnotationSnapshotRefDir)
	return ref
}

func (o *snapshotter) tryCommitWithRefPath(ctx context.Context, info snapshots.Info, name string, id string) ([]snapshots.Opt, error) {
	var opts []snapshots.Opt
	if o.useCubeRefPath {
		if ref, ok := info.Labels[constants.AnnotationSnapshotRef]; !ok || ref == "" {

			sname, err := parseSnapshotName(name)
			if err == nil {
				if sname.isDigest {
					if info.Labels == nil {
						info.Labels = make(map[string]string)
					}
					info.Labels[constants.AnnotationSnapshotRef] = sname.name
					oldSnapshotPath := filepath.Dir(o.upperPath(id))
					newRefPath, err := o.makeCubeRefPathDir(ctx, info)
					if err != nil {
						return nil, err
					}
					if newRefPath != "" {
						info.Labels[constants.AnnotationSnapshotRefDir] = newRefPath
						opts = append(opts, snapshots.WithLabels(info.Labels))
						if newRefPath != "" && newRefPath != oldSnapshotPath {
							if err := os.Rename(oldSnapshotPath, newRefPath); err != nil {
								return nil, fmt.Errorf("failed to rename: %w", err)
							}
						}
					}

					return opts, nil
				}
			}
		}
	}
	return opts, nil
}

func (o *snapshotter) getCleanupRefDirectories(ctx context.Context) ([]string, error) {
	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"method": "getCleanupRefDirectories",
	})
	var (
		snapshotMap = make(map[string]snapshots.Info)
		cleanup     = []string{}
	)
	err := storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
		sname, err := parseSnapshotName(info.Name)
		if err != nil {
			logEntry.WithError(err).Warnf("failed to parse snapshot name: %s", info.Name)
			return err
		}

		if ref, ok := info.Labels[constants.AnnotationSnapshotRef]; ok {
			cutref, _ := strings.CutPrefix(ref, constants.PrefixSha256)
			snapshotMap[sname.namespace+"/"+cutref] = info
		}
		snapshotMap[sname.namespace+"/"+sname.name] = info

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk storage snapshot info: %w", err)
	}

	refDir := filepath.Join(o.root, refDirName)
	fd, err := os.Open(refDir)
	if err != nil {
		if os.IsNotExist(err) {
			return cleanup, nil
		}
		return nil, err
	}
	defer fd.Close()

	nsdirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	for _, ns := range nsdirs {
		nsdir := filepath.Join(refDir, ns)
		nsfd, err := os.Open(nsdir)
		if err != nil {
			return nil, err
		}
		defer nsfd.Close()

		refdirs, err := nsfd.Readdirnames(0)
		if err != nil {
			return nil, err
		}
		for _, ref := range refdirs {
			refdir := filepath.Join(nsdir, ref)
			reffd, err := os.Open(refdir)
			if err != nil {
				logEntry.WithError(err).Warnf("failed to open ref dir %s", refdir)
				continue
			}
			defer reffd.Close()

			_, nsok := snapshotMap[ns+"/"+ref]

			if !nsok {
				logEntry.WithField("refdir", refdir).Info("add ref dir to cleanup")
				cleanup = append(cleanup, refdir)
			}
		}
	}

	return cleanup, nil
}

func removeDirectory(ctx context.Context, dir string) {
	if err := mount.UnmountRecursive(dir, 0); err != nil {
		log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to unmount directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
	}
}

type snapshotName struct {
	namespace string
	id        string
	name      string
	isDigest  bool
}

func parseSnapshotName(name string) (*snapshotName, error) {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) == 3 {
		return &snapshotName{
			namespace: parts[0],
			id:        parts[1],
			name:      parts[2],
			isDigest:  strings.HasPrefix(parts[2], constants.PrefixSha256),
		}, nil
	}
	return nil, fmt.Errorf("invalid snapshot name: %s", name)
}
