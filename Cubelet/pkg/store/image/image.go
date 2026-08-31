// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"k8s.io/apimachinery/pkg/util/sets"

	imagedigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/go-digest/digestset"
	imageidentity "github.com/opencontainers/image-spec/identity"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
)

type Image struct {
	ID string

	References []string

	ChainID string

	Size int64

	ImageSpec imagespec.Image

	Pinned bool

	LowerDirs []string
	UidFiles  string

	NfsRootfs string

	CosRootfs string

	ImageReq    *cubeimages.ImageSpec
	CreatedTime time.Time
}

type Store struct {
	lock sync.RWMutex

	refCache map[string]string

	client *containerd.Client

	store *store

	runtimeType string

	db *utils.CubeStore

	imageRefTimeMap map[string]time.Time

	uidFileDir string
}

var (
	DBBucketUsage = "usage/v1"
)

type Option func(*Store)

func WithUidFileDir(dir string) Option {
	return func(s *Store) {
		s.uidFileDir = dir
	}
}

func NewStore(client *containerd.Client, runtimeType string, db *utils.CubeStore, opts ...Option) *Store {
	s := &Store{
		refCache: make(map[string]string),
		client:   client,
		store: &store{
			images:    make(map[string]Image),
			digestSet: digestset.NewSet(),
		},
		runtimeType:     runtimeType,
		db:              db,
		imageRefTimeMap: make(map[string]time.Time),
	}
	for _, o := range opts {
		o(s)
	}
	if s.uidFileDir != "" {
		_ = os.MkdirAll(s.uidFileDir, os.ModeDir|0755)
	}
	go s.asyncFlushImageRefTime()
	return s
}

func (s *Store) Update(ctx context.Context, ref string) error {
	ctx = constants.WithRuntimeType(ctx, s.runtimeType)
	s.lock.Lock()
	defer s.lock.Unlock()
	i, err := s.client.GetImage(ctx, ref)
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("get image from containerd: %w", err)
	}
	var img *Image
	if err == nil {
		img, err = s.getImage(ctx, i)
		if err != nil {
			return fmt.Errorf("get image info from containerd: %w", err)
		}
	}

	toUpdateRef := sets.NewString(ref)
	if i != nil {
		repoDigest, repoTag := getImageRepoDigestAndTag(i)
		toUpdateRef.Insert(repoDigest, repoTag)
	}
	if img != nil {
		toUpdateRef.Insert(img.References...)
	}

	for _, ref := range toUpdateRef.List() {
		if ref == "" {
			continue
		}
		if err := s.update(ref, img); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) update(ref string, img *Image) error {
	oldID, oldExist := s.refCache[ref]
	if img == nil {

		if oldExist {

			s.store.delete(oldID, ref)
			delete(s.refCache, ref)
		}
		return nil
	}
	if oldExist {
		if oldID == img.ID {
			return nil
		}

		s.store.delete(oldID, ref)
	}

	s.refCache[ref] = img.ID
	return s.store.add(*img)
}

func (s *Store) getImage(ctx context.Context, i containerd.Image) (*Image, error) {

	diffIDs, err := i.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image diffIDs: %w", err)
	}
	chainID := imageidentity.ChainID(diffIDs)

	size, err := i.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image compressed resource size: %w", err)
	}

	pinned := false
	pin, ok := i.Metadata().Labels[constants.LabelPinnedImageKey]
	if ok && pin == constants.LabelPinnedImageValue {
		pinned = true
	}

	desc, err := i.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image config descriptor: %w", err)
	}
	id := desc.Digest.String()

	rb, err := content.ReadBlob(ctx, i.ContentStore(), desc)
	if err != nil {
		return nil, fmt.Errorf("read image config from content store: %w", err)
	}
	var ociimage imagespec.Image
	if err := json.Unmarshal(rb, &ociimage); err != nil {
		return nil, fmt.Errorf("unmarshal image config %s: %w", rb, err)
	}

	uidFiles := filepath.Join(s.uidFileDir, id)
	lowerDirs, err := s.prepareUidFiles(ctx, chainID.String(), uidFiles, i)
	if err != nil {
		return nil, fmt.Errorf("prepare uid files: %w", err)
	}
	return &Image{
		ID:         id,
		References: []string{i.Name()},
		ChainID:    chainID.String(),
		Size:       size,
		ImageSpec:  ociimage,
		UidFiles:   uidFiles,
		LowerDirs:  lowerDirs,
		Pinned:     pinned,
	}, nil
}

func (s *Store) UpdateWithCosImage(img *Image) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.update(img.ID, img)
}

func (s *Store) UpdateWithNfsImage(img *Image) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if img.NfsRootfs != "" {
		img.UidFiles = img.NfsRootfs
	}
	return s.update(img.ID, img)
}

func (s *Store) prepareUidFiles(ctx context.Context, parent, uidFiles string, img containerd.Image) (lowerDirs []string, retErr error) {
	snapshotter := s.client.SnapshotService(defaults.DefaultSnapshotter)
	if snapshotter == nil {
		return nil, fmt.Errorf("snapshotter %s was not found: %w", defaults.DefaultSnapshotter, errdefs.ErrNotFound)
	}
	key := utils.GenerateID()
	defer func() {
		if err := snapshotter.Remove(ctx, key); err != nil && !errdefs.IsNotFound(err) {
			CubeLog.Fatalf("getSnapshotLowerDir[%s],Error cleaning up snapshot after mount error:%v", img.Name(), err)
		}
	}()
	mounts, err := snapshotter.Prepare(ctx, key, parent)
	if err != nil {
		return nil, err
	}
	for _, option := range mounts[0].Options {
		if strings.HasPrefix(option, "lowerdir=") {
			ss := strings.Split(option, "=")
			if len(ss) < 2 {
				return nil, fmt.Errorf("invalid snapshot lowerdir option[%s]", ss)
			}
			lowerDirs = strings.Split(ss[1], ":")
			break
		}
	}
	if len(lowerDirs) == 0 {
		return nil, fmt.Errorf("invalid snapshot lowerdir option[%s]", mounts[0].Options)
	}

	if exist, size, _ := utils.FileExistWithSize(uidFiles); exist && size > 0 {
		return lowerDirs, nil
	}

	mounts = tryReadonlyMounts(mounts)

	defer func() {
		if retErr != nil {
			// NOCC:Path Traversal()
			if err := os.RemoveAll(uidFiles); err != nil {
				CubeLog.WithContext(ctx).Fatalf("oops, roll back rm dir[%s] failed. %s", uidFiles, err)
			}
		}
	}()

	return lowerDirs, mount.WithTempMount(ctx, mounts, func(root string) error {
		rootfsCacheDir := filepath.Join(uidFiles, "etc")
		if err := os.MkdirAll(rootfsCacheDir, os.ModeDir|0755); err != nil {
			return err
		}
		uidSource := filepath.Join(root, "/etc/passwd")
		uidFile := filepath.Join(rootfsCacheDir, "passwd")
		if err := utils.SafeCopyFile(uidFile, uidSource); err != nil && !os.IsNotExist(err) {
			return err
		}

		gidSource := filepath.Join(root, "/etc/group")
		gidFile := filepath.Join(rootfsCacheDir, "group")
		if err := utils.SafeCopyFile(gidFile, gidSource); err != nil && !os.IsNotExist(err) {
			return err
		}

		return nil
	})
}

func tryReadonlyMounts(mounts []mount.Mount) []mount.Mount {
	if len(mounts) == 1 && mounts[0].Type == "overlay" {
		mounts[0].Options = append(mounts[0].Options, "ro")
	}
	return mounts
}

func (s *Store) Resolve(ref string) (string, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	id, ok := s.refCache[ref]
	if !ok {
		return "", errdefs.ErrNotFound
	}
	return id, nil
}

func (s *Store) Get(id string) (Image, error) {
	return s.store.get(id)
}

func (s *Store) List() []Image {
	return s.store.list()
}

func (s *Store) asyncFlushImageRefTime() {
	tick := time.NewTicker(10 * time.Minute)
	for range tick.C {
		s.FlushImageRefTime()
	}
}

func (s *Store) FlushImageRefTime() {
	s.lock.Lock()
	defer s.lock.Unlock()

	var err error
	for id, latest := range s.imageRefTimeMap {
		lastRef := []byte(strconv.Itoa(int(latest.Unix())))
		for i := 0; i < 3; i++ {
			if err = s.db.Set(DBBucketUsage, id, lastRef); err == nil {
				continue
			}
			time.Sleep(5 * time.Millisecond)
		}

	}
}

func (s *Store) UpdateLastRefTime(refOrID string) {
	id, err := s.Resolve(refOrID)
	if errdefs.IsNotFound(err) {
		if _, err := s.store.digestSet.Lookup(refOrID); err != nil {
			return
		}
		id = refOrID
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	s.imageRefTimeMap[id] = time.Now()
}

func (s *Store) GetLastRefTime(id string) time.Time {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.imageRefTimeMap[id]
}

func (s *Store) DeleteLastRefTime(refOrID string) {
	id, err := s.Resolve(refOrID)
	if errdefs.IsNotFound(err) {
		if _, err := s.store.digestSet.Lookup(refOrID); err != nil {
			return
		}
		id = refOrID
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.imageRefTimeMap, id)

	for i := 0; i < 3; i++ {
		if err = s.db.Delete(DBBucketUsage, id); err == nil {
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Store) RecoverRefTime() error {
	refMap, err := s.db.ReadAll(DBBucketUsage)
	if err != nil {
		return err
	}
	s.lock.Lock()
	for imageId, refTimeStr := range refMap {
		unixTime, err := strconv.ParseInt(string(refTimeStr), 10, 64)
		if err != nil {
			CubeLog.Errorf("invalid data %q, skip", refTimeStr)
			s.imageRefTimeMap[imageId] = time.Now()
			continue
		}
		refTime := time.Unix(unixTime, 0)
		s.imageRefTimeMap[imageId] = refTime
		CubeLog.Debugf("Loaded image ref time for %q: %v", imageId, refTime.Format(time.RFC3339))
	}

	for _, i := range s.store.images {
		if _, ok := s.imageRefTimeMap[i.ID]; !ok {
			s.imageRefTimeMap[i.ID] = time.Now()
		}
	}
	s.lock.Unlock()

	s.FlushImageRefTime()
	return nil
}

type store struct {
	lock      sync.RWMutex
	images    map[string]Image
	digestSet *digestset.Set
}

func (s *store) list() []Image {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var images []Image
	for _, i := range s.images {
		images = append(images, i)
	}
	return images
}

func (s *store) add(img Image) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, err := s.digestSet.Lookup(img.ID); err != nil {
		if err != digestset.ErrDigestNotFound {
			return err
		}
		if err := s.digestSet.Add(imagedigest.Digest(img.ID)); err != nil {
			return err
		}
	}

	i, ok := s.images[img.ID]
	if !ok {

		s.images[img.ID] = img
		return nil
	}

	i.References = sortReferences(util.MergeStringSlices(i.References, img.References))
	s.images[img.ID] = i
	return nil
}

func (s *store) get(id string) (Image, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		if err == digestset.ErrDigestNotFound {
			err = errdefs.ErrNotFound
		}
		return Image{}, err
	}
	if i, ok := s.images[digest.String()]; ok {
		return i, nil
	}
	return Image{}, errdefs.ErrNotFound
}

func (s *store) delete(id, ref string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {

		return
	}
	i, ok := s.images[digest.String()]
	if !ok {
		return
	}
	i.References = util.SubtractStringSlice(i.References, ref)
	if len(i.References) != 0 {
		s.images[digest.String()] = i
		return
	}

	s.digestSet.Remove(digest)
	delete(s.images, digest.String())

	if i.NfsRootfs == i.UidFiles {
		return
	}
	if err = os.RemoveAll(i.UidFiles); err != nil {
		CubeLog.Errorf("fail to delete uid files %v from image cache", i.UidFiles)
	}
}

func getImageRepoDigestAndTag(i containerd.Image) (string, string) {
	named, err := reference.ParseDockerRef(i.Name())
	if err != nil {
		return i.Name(), ""
	}
	target := i.Target().Digest
	return util.GetRepoDigestAndTag(named, target)
}
