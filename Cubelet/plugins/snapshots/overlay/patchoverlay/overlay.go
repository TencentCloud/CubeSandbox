// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package patchoverlay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/sirupsen/logrus"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

const (
	upperdirKey = "containerd.io/snapshot/overlay.upperdir"
)

type SnapshotterConfig struct {
	asyncRemove   bool
	upperdirLabel bool
	ms            MetaStore
	mountOptions  []string
	remapIDs      bool
	slowChown     bool

	useCubeRefPath bool
}

type Opt func(config *SnapshotterConfig) error

func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

func WithUpperdirLabel(config *SnapshotterConfig) error {
	config.upperdirLabel = true
	return nil
}

func WithMountOptions(options []string) Opt {
	return func(config *SnapshotterConfig) error {
		config.mountOptions = append(config.mountOptions, options...)
		return nil
	}
}

type MetaStore interface {
	TransactionContext(ctx context.Context, writable bool) (context.Context, storage.Transactor, error)
	WithTransaction(ctx context.Context, writable bool, fn storage.TransactionCallback) error
	Close() error
}

func WithMetaStore(ms MetaStore) Opt {
	return func(config *SnapshotterConfig) error {
		config.ms = ms
		return nil
	}
}

func WithRemapIDs(config *SnapshotterConfig) error {
	config.remapIDs = true
	return nil
}

func WithSlowChown(config *SnapshotterConfig) error {
	config.slowChown = true
	return nil
}

type snapshotter struct {
	root          string
	ms            MetaStore
	asyncRemove   bool
	upperdirLabel bool
	options       []string
	remapIDs      bool
	slowChown     bool

	useCubeRefPath bool
}

func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	if config.ms == nil {
		config.ms, err = storage.NewMetaStore(filepath.Join(root, "metadata.db"))
		if err != nil {
			return nil, err
		}
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if config.useCubeRefPath {
		if err := os.MkdirAll(filepath.Join(root, refDirName, namespaces.Default), 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}

	if !hasOption(config.mountOptions, "userxattr", false) {

		userxattr, err := overlayutils.NeedsUserXAttr(root)
		if err != nil {
			log.L.WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
		}
		if userxattr {
			config.mountOptions = append(config.mountOptions, "userxattr")
		}
	}

	if !hasOption(config.mountOptions, "index", false) && supportsIndex() {
		config.mountOptions = append(config.mountOptions, "index=off")
	}

	s := &snapshotter{
		root:          root,
		ms:            config.ms,
		asyncRemove:   config.asyncRemove,
		upperdirLabel: config.upperdirLabel,
		options:       config.mountOptions,
		remapIDs:      config.remapIDs,
		slowChown:     config.slowChown,

		useCubeRefPath: config.useCubeRefPath,
	}

	err = s.ms.WithTransaction(context.TODO(), false, func(ctx context.Context) error {
		idmaps, err := storage.IDMap(ctx)
		if err != nil {
			return fmt.Errorf("failed to get idmaps: %w", err)
		}
		infoCount := 0
		err = storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
			infoCount++
			stepLog := log.G(ctx).WithFields(log.Fields{
				"info":        info,
				"snapshotter": "overlay",
				"method":      "NewSnapshotter",
			})
			s, e := storage.GetSnapshot(ctx, info.Name)
			if e != nil {
				stepLog.WithError(err).Errorf("failed to get storage snapshot")
				return nil
			}

			stepLog.WithField("snapshot", s).Debugf("detected existing snapshot %s", info.Name)
			return nil
		})
		if infoCount != len(idmaps) {
			log.L.Fatalf("overlay snapshot count %d does not match idmap count %d", infoCount, len(idmaps))
		} else {
			log.L.Infof("overlay snapshot count %d matches idmap count %d", infoCount, len(idmaps))
		}
		return err
	})
	if err != nil {
		log.L.WithError(err).Errorf("failed to walk storage info when initializing overlay snapshotter")
	} else {
		log.L.Infof("overlay snapshotter initialized with config: %+v", config)
	}
	return s, nil
}

func hasOption(options []string, key string, hasValue bool) bool {
	for _, option := range options {
		if hasValue {
			if strings.HasPrefix(option, key) && len(option) > len(key) && option[len(key)] == '=' {
				return true
			}
		} else if option == key {
			return true
		}
	}
	return false
}

func (o *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	var id string
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, _, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return info, err
	}

	if o.upperdirLabel {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.getValidCubeUpperPath(ctx, id, info)
	}
	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (newInfo snapshots.Info, err error) {
	err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		newInfo, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		if err != nil {
			return err
		}

		if o.upperdirLabel {
			id, _, _, err := storage.GetInfo(ctx, newInfo.Name)
			if err != nil {
				return err
			}
			if newInfo.Labels == nil {
				newInfo.Labels = make(map[string]string)
			}
			newInfo.Labels[upperdirKey] = o.getValidCubeUpperPath(ctx, id, newInfo)
		}
		return nil
	})
	return newInfo, err
}

func (o *snapshotter) Usage(ctx context.Context, key string) (_ snapshots.Usage, err error) {
	var (
		usage snapshots.Usage
		info  snapshots.Info
		id    string
	)
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return usage, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := o.getValidCubeUpperPath(ctx, id, info)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {

			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

func (o *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	var s storage.Snapshot
	var info snapshots.Info
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		s, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get active mount: %w", err)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return o.mounts(ctx, s, info), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {

	info, err := o.Stat(ctx, key)
	if err != nil {
		return err
	}

	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return err
		}
	}
	for k, v := range base.Labels {
		info.Labels[k] = v
	}
	opts = append(opts, snapshots.WithLabels(info.Labels))
	return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {

		id, info, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info with key %q: %w", key, err)
		}

		var usage fs.Usage
		if v, ok := info.Labels[constants.AnnotationSnapshotterCustomUsage]; ok {
			err = json.Unmarshal([]byte(v), &usage)
		} else {
			usage, err = fs.DiskUsage(ctx, o.getValidCubeUpperPath(ctx, id, info))
		}

		if err != nil {
			return err
		}

		externalOpts, err := o.tryCommitWithRefPath(ctx, info, name, id)
		if err != nil {
			return err
		}
		opts = append(opts, externalOpts...)

		if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}
		return nil
	})
}

func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	var removals []string

	defer func() {
		if err == nil {
			for _, dir := range removals {
				removeDirectory(ctx, dir)
			}
		}
	}()
	return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, _, err = storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
		}

		if !o.asyncRemove {
			removals, err = o.getCleanupDirectories(ctx)
			if err != nil {
				return fmt.Errorf("unable to get directories for removal: %w", err)
			}
		}
		return nil
	})
}

func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	return o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		if o.upperdirLabel {
			return storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
				id, _, _, err := storage.GetInfo(ctx, info.Name)
				if err != nil {
					return err
				}
				if info.Labels == nil {
					info.Labels = make(map[string]string)
				}
				info.Labels[upperdirKey] = o.getValidCubeUpperPath(ctx, id, info)
				return fn(ctx, info)
			}, fs...)
		}
		return storage.WalkInfo(ctx, fn, fs...)
	})
}

func (o *snapshotter) Cleanup(ctx context.Context) error {
	cleanup, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}

	for _, dir := range cleanup {
		removeDirectory(ctx, dir)
	}

	return nil
}

func (o *snapshotter) cleanupDirectories(ctx context.Context) (_ []string, err error) {
	var cleanupDirs []string

	if err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		cleanupDirs, err = o.getCleanupDirectories(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return cleanupDirs, nil
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	refCleanup, err := o.getCleanupRefDirectories(ctx)
	if err != nil {
		return nil, err
	}

	refCleanup = append(refCleanup, cleanup...)
	return refCleanup, nil
}

func validateIDMapping(mapping string) error {
	var (
		hostID int
		ctrID  int
		length int
	)

	if _, err := fmt.Sscanf(mapping, "%d:%d:%d", &ctrID, &hostID, &length); err != nil {
		return err
	}

	if ctrID < 0 || hostID < 0 || length < 0 {
		return fmt.Errorf("invalid mapping \"%d:%d:%d\"", ctrID, hostID, length)
	}
	if ctrID != 0 {
		return fmt.Errorf("container mapping of 0 is only supported")
	}
	return nil
}

func hostID(mapping string) (int, error) {
	var (
		hostID int
		ctrID  int
		length int
	)
	if err := validateIDMapping(mapping); err != nil {
		return -1, fmt.Errorf("invalid mapping: %w", err)
	}
	if _, err := fmt.Sscanf(mapping, "%d:%d:%d", &ctrID, &hostID, &length); err != nil {
		return -1, err
	}
	return hostID, nil
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		s        storage.Snapshot
		td, path string
		info     snapshots.Info
	)

	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, err
		}
	}
	ref := base.Labels[constants.AnnotationSnapshotRef]
	refkey := ""
	if ref != "" {
		sname, err := parseSnapshotName(key)
		if err == nil {
			refkey = sname.namespace + "/" + sname.id + "/" + ref
		}
	}
	stepLog := log.G(ctx).WithFields(log.Fields{
		"method": "snapshotter.createSnapshot",
		"key":    key,
		"parent": parent,
		"ref":    ref,
		"refkey": refkey,
	})

	refpathExist := false
	shouldCommit := false
	cubeRefPath, err := o.makeCubeRefPathDir(ctx, base)
	if err == nil && cubeRefPath != "" {
		opts = append(opts, snapshots.WithLabels(map[string]string{
			constants.AnnotationSnapshotRefDir: cubeRefPath,
		}))
	} else if errdefs.IsAlreadyExists(err) {
		opts = append(opts, snapshots.WithLabels(map[string]string{
			constants.AnnotationSnapshotRefDir: cubeRefPath,
		}))
		refpathExist = true
	} else if err != nil {
		return nil, err
	}
	stepLog.Debugf("start to create snapshot")

	defer func() {
		if td != "" {
			if err1 := os.RemoveAll(td); err1 != nil {
				log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
			}
		}
		if err != nil && !errdefs.IsAlreadyExists(err) {
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}

		if refpathExist && shouldCommit && ref != "" {
			e := o.Commit(ctx, refkey, key, opts...)
			if e != nil {
				log.G(ctx).WithError(e).Warn("failed to commit snapshot")
			} else {
				stepLog.Infof("commit snapshot %s to %s", key, ref)
			}
		}
	}()

	if err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) (err error) {
		snapshotDir := filepath.Join(o.root, "snapshots")
		if !refpathExist {
			td, err = o.prepareDirectory(ctx, snapshotDir, kind)
			if err != nil {
				return fmt.Errorf("failed to create prepare snapshot dir: %w", err)
			}
		} else {
			_, err := storage.GetSnapshot(ctx, refkey)
			if err == nil {
				return fmt.Errorf("committed cube ref snapshot already exist: %w", errdefs.ErrAlreadyExists)
			}
			shouldCommit = true
		}

		s, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}
		if refpathExist {
			return fmt.Errorf("refpath already exist: %w", errdefs.ErrAlreadyExists)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}

		mappedUID := -1
		mappedGID := -1

		if v, ok := info.Labels[snapshots.LabelSnapshotUIDMapping]; ok {
			if mappedUID, err = hostID(v); err != nil {
				return fmt.Errorf("failed to parse UID mapping: %w", err)
			}
		}
		if v, ok := info.Labels[snapshots.LabelSnapshotGIDMapping]; ok {
			if mappedGID, err = hostID(v); err != nil {
				return fmt.Errorf("failed to parse GID mapping: %w", err)
			}
		}

		if mappedUID == -1 || mappedGID == -1 {
			if len(s.ParentIDs) > 0 {
				_, pinfo, _, err := storage.GetInfo(ctx, info.Parent)
				if err != nil {
					return fmt.Errorf("failed to get snapshot info: %w", err)
				}
				st, err := os.Stat(o.getValidCubeUpperPath(ctx, s.ParentIDs[0], pinfo))
				if err != nil {
					return fmt.Errorf("failed to stat parent: %w", err)
				}
				stat, ok := st.Sys().(*syscall.Stat_t)
				if !ok {
					return fmt.Errorf("incompatible types after stat call: *syscall.Stat_t expected")
				}
				mappedUID = int(stat.Uid)
				mappedGID = int(stat.Gid)
			}
		}

		if mappedUID != -1 && mappedGID != -1 {
			if err := os.Lchown(filepath.Join(td, "fs"), mappedUID, mappedGID); err != nil {
				return fmt.Errorf("failed to chown: %w", err)
			}
		}

		path = filepath.Join(snapshotDir, s.ID)

		if cubeRefPath != "" {
			stepLog.WithFields(logrus.Fields{
				"snapshot_id": s.ID,
				"path":        path,
				"snapshot":    info.Name,
				"label":       info.Labels,
			}).Info("use cube ref path")
			path = cubeRefPath
		} else {
			stepLog.WithFields(logrus.Fields{
				"snapshot_id": s.ID,
				"path":        path,
				"snapshot":    info.Name,
				"label":       info.Labels,
			}).Info("use snapshot dir")
		}

		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("snapshot failed to rename: %w", err)
		}
		td = ""
		path = ""

		return nil
	}); err != nil {
		return nil, err
	}
	return o.mounts(ctx, s, info), nil
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (o *snapshotter) mounts(ctx context.Context, s storage.Snapshot, info snapshots.Info) []mount.Mount {
	var options []string

	if o.remapIDs {
		if v, ok := info.Labels[snapshots.LabelSnapshotUIDMapping]; ok {
			options = append(options, fmt.Sprintf("uidmap=%s", v))
		}
		if v, ok := info.Labels[snapshots.LabelSnapshotGIDMapping]; ok {
			options = append(options, fmt.Sprintf("gidmap=%s", v))
		}
	}

	if len(s.ParentIDs) == 0 {

		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}
		return []mount.Mount{
			{
				Source: o.getValidCubeUpperPath(ctx, s.ID, info),
				Type:   "bind",
				Options: append(options,
					roFlag,
					"rbind",
				),
			},
		}
	}

	stepLogger := log.G(ctx).WithFields(logrus.Fields{
		"step": "mounts",
		"info": info.Name,
	})

	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.getCubeWorkPath(ctx, s.ID, info)),
			fmt.Sprintf("upperdir=%s", o.getValidCubeUpperPath(ctx, s.ID, info)),
		)
	} else if len(s.ParentIDs) == 1 {
		source := o.upperPath(s.ParentIDs[0])

		pInfo, err := o.Stat(ctx, info.Parent)
		if err != nil {
			stepLogger.WithError(err).Warnf("failed to get snapshot info")
		} else {
			newSource := o.getValidCubeUpperPath(ctx, s.ParentIDs[0], pInfo)
			if newSource != "" {
				source = newSource
			}
		}
		return []mount.Mount{
			{
				Source: source,
				Type:   "bind",
				Options: append(options,
					"ro",
					"rbind",
				),
			},
		}
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}

	if info.Parent != "" {
		externalParentPaths := make([]string, len(s.ParentIDs))
		tempInfo, err := o.Stat(ctx, info.Parent)
		if err == nil {
			for i := range s.ParentIDs {
				externalParentPaths[i] = o.getValidCubeUpperPath(ctx, s.ParentIDs[i], tempInfo)
				if tempInfo.Parent == "" {
					break
				}
				tempInfo, err = o.Stat(ctx, tempInfo.Parent)
				if err != nil {
					err = fmt.Errorf("failed to get snapshot %s: %w", tempInfo.Parent, err)
					break
				}
			}
			if err == nil {
				parentPaths = externalParentPaths
			} else {
				log.G(ctx).WithError(err).Warnf("failed to get external path for snapshot %s", info.Name)
			}
		}
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	options = append(options, o.options...)

	return []mount.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}
}

func (o *snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

func (o *snapshotter) Close() error {
	return o.ms.Close()
}

func supportsIndex() bool {
	if _, err := os.Stat("/sys/module/overlay/parameters/index"); err == nil {
		return true
	}
	return false
}
