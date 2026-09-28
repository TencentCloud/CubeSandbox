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
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/usage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	docker "github.com/distribution/reference"
	imagedigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/go-digest/digestset"
	imageidentity "github.com/opencontainers/image-spec/identity"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/labels"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

type Image struct {
	ID string

	References []string

	ChainID   string
	Snapshots []string

	Size int64

	ImageSpec imagespec.Image

	Pinned bool

	UidFiles string

	MediaType   string
	Annotation  map[string]string
	CreatedTime time.Time
	Namespace   string

	HostLayers []string
}

type Getter interface {
	Get(ctx context.Context, name string) (images.Image, error)
}

type Store struct {
	lock sync.RWMutex

	images Getter

	provider content.InfoReaderProvider

	platform platforms.MatchComparer

	nsStore map[string]*store

	uidFileDir string
}

func NewStore(img Getter, provider content.InfoReaderProvider, platform platforms.MatchComparer) *Store {
	return &Store{
		images:   img,
		provider: provider,
		platform: platform,
		nsStore:  make(map[string]*store),
	}
}

func (s *Store) Update(ctx context.Context, ref string) error {
	i, err := s.images.Get(ctx, ref)
	if errdefs.IsNotFound(err) {
		log.G(ctx).Debugf("update cri image %s not found, will remove from cache", ref)
	} else if err != nil {
		return fmt.Errorf("get image from containerd: %w", err)
	}

	var img *Image
	if err == nil {
		img, err = s.getImage(ctx, i)
		if err != nil {
			return fmt.Errorf("get image info from containerd: %w", err)
		}
	}
	return s.update(ctx, ref, img)
}

func (s *Store) getNamespaceStore(ctx context.Context) (*store, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, fmt.Errorf("image store get namespace failed: %w", err)
	}
	s.lock.Lock()
	defer s.lock.Unlock()

	store, ok := s.nsStore[ns]
	if !ok {
		s.nsStore[ns] = newNsStore(ns)
		store = s.nsStore[ns]
	}
	return store, nil
}

func (s *Store) update(ctx context.Context, ref string, img *Image) error {
	store, err := s.getNamespaceStore(ctx)
	if err != nil {
		return err
	}
	oldID, oldExist := store.getRef(ref)
	if img == nil {

		if oldExist {
			log.G(ctx).Debugf("image %s with oldID %s not found in containerd, will remove from cache", ref, oldID)

			store.delete(oldID, ref)
			store.deleteRef(ref)
		}
		return nil
	}
	if oldExist {
		if oldID == img.ID {
			if store.isPinned(img.ID, ref) == img.Pinned {
				return nil
			}
			if img.Pinned {
				return store.pin(img.ID, ref)
			}
			return store.unpin(img.ID, ref)
		}

		store.delete(oldID, ref)
	}

	store.setRef(ref, img.ID)
	return store.add(*img)
}

func (s *Store) getImage(ctx context.Context, i images.Image) (*Image, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		ns = namespaces.Default
	}
	diffIDs, err := i.RootFS(ctx, s.provider, s.platform)
	if err != nil {
		return nil, fmt.Errorf("get image diffIDs: %w", err)
	}
	chainID := imageidentity.ChainID(diffIDs)
	var sns []string
	if v, ok := i.Labels[constants.LabelImageLayerDirs]; ok {
		err = json.Unmarshal([]byte(v), &sns)
		if err != nil {
			log.G(ctx).Errorf("unmarshal image layer dirs failed: %v", err)
		}
	} else {
		for i := range diffIDs {
			sns = append(sns, imageidentity.ChainID(diffIDs[:len(diffIDs)-i]).String())
		}
	}

	size, err := usage.CalculateImageUsage(ctx, i, s.provider, usage.WithManifestLimit(s.platform, 1), usage.WithManifestUsage())
	if err != nil {
		return nil, fmt.Errorf("get image compressed resource size: %w", err)
	}

	desc, err := i.Config(ctx, s.provider, s.platform)
	if err != nil {
		return nil, fmt.Errorf("get image config descriptor: %w", err)
	}
	id := desc.Digest.String()

	blob, err := content.ReadBlob(ctx, s.provider, desc)
	if err != nil {
		return nil, fmt.Errorf("read image config from content store: %w", err)
	}

	var spec imagespec.Image
	if err := json.Unmarshal(blob, &spec); err != nil {
		return nil, fmt.Errorf("unmarshal image config %s: %w", blob, err)
	}

	pinned := i.Labels[labels.PinnedImageLabelKey] == labels.PinnedImageLabelValue
	if !pinned {
		pinned = i.Labels[constants.LabelPinnedImageKey] == constants.LabelPinnedImageValue
	}

	media := cubeimages.ImageStorageMediaType_docker.String()
	if v, ok := i.Labels[constants.LabelContainerImageMedia]; ok {
		media = v
	}

	var hostLayers []string
	if v := i.Labels[constants.LabelImageNoHostLayers]; v == constants.StringTrueValue {

		hostLayers = []string{}
	} else if v, ok := i.Labels[constants.LabelImageLayerDirs]; ok {
		var layers []string
		err = json.Unmarshal([]byte(v), &layers)
		if err != nil {
			log.G(ctx).Errorf("unmarshal image layer dirs failed: %v", err)
		}
		hostLayers = append(hostLayers, layers...)
		if prefix, ok := i.Labels[constants.LabelImageHostLowerDirsPrefix]; ok {
			for i := range hostLayers {
				if !filepath.IsAbs(hostLayers[i]) {
					hostLayers[i] = filepath.Join(prefix, hostLayers[i])
				}
			}
		}
	} else {
		if v, ok := i.Labels[constants.LabelImageHostLowerDirs]; ok {
			err = json.Unmarshal([]byte(v), &hostLayers)
			if err != nil {
				log.G(ctx).Errorf("unmarshal image host lower dirs failed: %v", err)
			}
		}
	}

	return &Image{
		ID:          id,
		References:  []string{i.Name},
		ChainID:     chainID.String(),
		Snapshots:   sns,
		Size:        size,
		ImageSpec:   spec,
		Pinned:      pinned,
		UidFiles:    i.Labels[constants.LabelImageUidFiles],
		CreatedTime: i.CreatedAt,
		MediaType:   media,
		Annotation:  i.Labels,
		Namespace:   ns,
		HostLayers:  hostLayers,
	}, nil

}

func (s *Store) Resolve(ctx context.Context, ref string) (string, error) {
	store, err := s.getNamespaceStore(ctx)
	if err != nil {
		return "", err
	}

	id, ok := store.getRef(ref)
	if !ok {
		return "", errdefs.ErrNotFound
	}
	return id, nil
}

func (s *Store) Get(ctx context.Context, id string) (Image, error) {
	store, err := s.getNamespaceStore(ctx)
	if err != nil {
		return Image{}, err
	}
	return store.get(id)
}

func (s *Store) List(ctx context.Context) ([]Image, error) {
	_, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		var images []Image

		for _, store := range s.nsStore {
			images = append(images, store.list()...)
		}
		return images, nil
	}
	store, err := s.getNamespaceStore(ctx)
	if err != nil {
		return nil, err
	}
	return store.list(), nil
}

func (s *Store) ListAllNamespaceImage() map[string][]Image {
	nsImagesMap := make(map[string][]Image)
	for ns, store := range s.nsStore {
		nsImagesMap[ns] = store.list()
	}
	return nsImagesMap
}

type store struct {
	lock       sync.RWMutex
	ns         string
	images     map[string]Image
	digestSet  *digestset.Set
	pinnedRefs map[string]sets.Set[string]

	refCache map[string]string
}

func newNsStore(ns string) *store {
	return &store{
		ns:         ns,
		images:     make(map[string]Image),
		digestSet:  digestset.NewSet(),
		pinnedRefs: make(map[string]sets.Set[string]),
		refCache:   make(map[string]string),
	}
}

func (s *store) ID() string {
	return s.ns
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

	if img.Pinned {
		if refs := s.pinnedRefs[img.ID]; refs == nil {
			s.pinnedRefs[img.ID] = sets.New(img.References...)
		} else {
			refs.Insert(img.References...)
		}
	}

	i, ok := s.images[img.ID]
	if !ok {

		s.images[img.ID] = img
		return nil
	}

	i.References = docker.Sort(util.MergeStringSlices(i.References, img.References))
	i.Pinned = i.Pinned || img.Pinned
	s.images[img.ID] = i
	return nil
}

func (s *store) isPinned(id, ref string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		return false
	}
	refs := s.pinnedRefs[digest.String()]
	return refs != nil && refs.Has(ref)
}

func (s *store) pin(id, ref string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		if err == digestset.ErrDigestNotFound {
			err = errdefs.ErrNotFound
		}
		return err
	}
	i, ok := s.images[digest.String()]
	if !ok {
		return errdefs.ErrNotFound
	}

	if refs := s.pinnedRefs[digest.String()]; refs == nil {
		s.pinnedRefs[digest.String()] = sets.New(ref)
	} else {
		refs.Insert(ref)
	}
	i.Pinned = true
	s.images[digest.String()] = i
	return nil
}

func (s *store) unpin(id, ref string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		if err == digestset.ErrDigestNotFound {
			err = errdefs.ErrNotFound
		}
		return err
	}
	i, ok := s.images[digest.String()]
	if !ok {
		return errdefs.ErrNotFound
	}

	refs := s.pinnedRefs[digest.String()]
	if refs == nil {
		return nil
	}
	if refs.Delete(ref); len(refs) > 0 {
		return nil
	}

	delete(s.pinnedRefs, digest.String())
	i.Pinned = false
	s.images[digest.String()] = i
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
		if refs := s.pinnedRefs[digest.String()]; refs != nil {
			if refs.Delete(ref); len(refs) == 0 {
				i.Pinned = false

				delete(s.pinnedRefs, digest.String())
			}
		}

		s.images[digest.String()] = i
		return
	}

	s.digestSet.Remove(digest)
	delete(s.images, digest.String())
	delete(s.pinnedRefs, digest.String())
}

func (s *store) setRef(ref, id string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.refCache[ref] = id
}

func (s *store) getRef(ref string) (string, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	v, ok := s.refCache[ref]
	return v, ok
}

func (s *store) deleteRef(ref string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.refCache, ref)
}
