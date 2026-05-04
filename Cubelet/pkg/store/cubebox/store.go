// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"

	"encoding/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
)

var (
	DBBucketSandbox = "sandbox/v1"
)

type Store struct {
	indexer cache.Indexer

	db multimeta.MetadataDBAPI
}

var (
	templateIDIndexerKey = "templateID"

	containerIDIndexerKey = "containerID"

	baseBlockIDIndexerKey = "baseblockID"

	imageIDIndexerKey = "imageID"

	indexers = cache.Indexers{
		templateIDIndexerKey: func(obj any) ([]string, error) {
			cb, ok := obj.(*CubeBox)
			if !ok {
				return nil, errors.New("obj is not a cubebox")
			}
			if cb.LocalRunTemplate != nil {
				return []string{cb.LocalRunTemplate.TemplateID}, nil
			}
			return []string{}, nil
		},
		containerIDIndexerKey: func(obj any) ([]string, error) {
			cb, ok := obj.(*CubeBox)
			if !ok {
				return nil, errors.New("obj is not a cubebox")
			}
			var cids []string
			for _, c := range cb.AllContainers() {
				cids = append(cids, c.ID)
			}

			return cids, nil
		},
		baseBlockIDIndexerKey: func(obj any) ([]string, error) {
			cb, ok := obj.(*CubeBox)
			if !ok {
				return nil, errors.New("obj is not a cubebox")
			}

			var blockIDs []string
			if cb.LocalRunTemplate == nil {
				return blockIDs, nil
			}
			for _, v := range cb.LocalRunTemplate.Volumes {
				if v.VolumeID != "" {
					blockIDs = append(blockIDs, v.VolumeID)
				}
			}
			return blockIDs, nil
		},
		imageIDIndexerKey: func(obj any) ([]string, error) {
			cb, ok := obj.(*CubeBox)
			if !ok {
				return nil, errors.New("obj is not a cubebox")
			}

			var imageIDs sets.Set[string] = sets.New[string]()

			for _, c := range cb.AllContainers() {
				imageIDs.Insert(c.GetContainerImageIDs()...)
			}
			for _, imageReference := range cb.ImageReferences {
				imageIDs.Insert(imageReference.ID)
				imageIDs.Insert(imageReference.References...)
			}
			return imageIDs.UnsortedList(), nil
		},
	}
)

func cubeboxKeyFunc(obj any) (string, error) {
	cb, ok := obj.(*CubeBox)
	if !ok {
		return "", errors.New("obj is not a cubebox")
	}
	return cb.ID, nil
}

func NewStore(db multimeta.MetadataDBAPI) *Store {
	return &Store{
		db:      db,
		indexer: cache.NewIndexer(cubeboxKeyFunc, indexers),
	}
}

func (s *Store) Add(box *CubeBox) {
	s.indexer.Update(box)
}

func (s *Store) Get(id string) (*CubeBox, error) {
	obj, exist, err := s.indexer.GetByKey(id)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, utils.ErrorKeyNotFound
	}
	cb, ok := obj.(*CubeBox)
	if !ok {
		return nil, errors.New("obj is not a cubebox")
	}
	return cb, nil
}

func (s *Store) GetContainer(id string) (*Container, error) {

	objs, err := s.indexer.ByIndex(containerIDIndexerKey, id)
	if err != nil {
		return &Container{}, err
	}
	if len(objs) == 0 {
		return &Container{}, utils.ErrorKeyNotFound
	}

	cb, ok := objs[0].(*CubeBox)
	if !ok {
		return &Container{}, errors.New("obj is not a cubebox")
	}

	c, ok := cb.AllContainers()[id]
	if !ok {
		return &Container{}, utils.ErrorKeyNotFound
	}
	return c, nil
}

func (s *Store) DeleteContainer(id string) {

	objs, err := s.indexer.ByIndex(containerIDIndexerKey, id)
	if err != nil || len(objs) == 0 {
		return
	}

	box, ok := objs[0].(*CubeBox)
	if !ok {
		return
	}

	box.DeleteContainer(id)

	s.indexer.Update(box)
}

func (s *Store) List() []*CubeBox {
	objs := s.indexer.List()
	boxes := make([]*CubeBox, 0, len(objs))
	for _, obj := range objs {
		if cb, ok := obj.(*CubeBox); ok {
			boxes = append(boxes, cb)
		}
	}
	return boxes
}

func (s *Store) Len() int {
	return len(s.indexer.List())
}

func (s *Store) Sync(id string) error {
	var sb *CubeBox

	obj, exist, err := s.indexer.GetByKey(id)
	if err != nil {
		return err
	}

	if exist {
		var ok bool
		sb, ok = obj.(*CubeBox)
		if !ok {
			return errors.New("obj is not a cubebox")
		}
	} else {

		objs, err := s.indexer.ByIndex(containerIDIndexerKey, id)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		var ok bool
		sb, ok = objs[0].(*CubeBox)
		if !ok {
			return errors.New("obj is not a cubebox")
		}
	}

	sbCopy := s.createSafeCopy(sb)

	bs, err := json.Marshal(sbCopy)
	if err != nil {
		return err
	}

	return s.db.SetWithTx(DBBucketSandbox, sb.ID, bs, nil)
}

func (s *Store) createSafeCopy(sb *CubeBox) *CubeBox {
	if sb == nil {
		return nil
	}

	return sb.DeepCopy()
}

func (s *Store) DeleteSync(id string) error {

	if err := s.db.DeleteWithTx(DBBucketSandbox, id, func() error {

		obj, exist, _ := s.indexer.GetByKey(id)
		if exist {
			return s.indexer.Delete(obj)
		}
		return nil
	}); err != nil && err != utils.ErrorKeyNotFound &&
		err != utils.ErrorBucketNotFound {
		return err
	}
	return nil
}

func (s *Store) getCubeboxByIndex(index string, key string) ([]*CubeBox, error) {
	var cbs []*CubeBox
	objs, err := s.indexer.ByIndex(index, key)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if cb, ok := obj.(*CubeBox); ok {
			cbs = append(cbs, cb)
		} else {
			return nil, errors.New("obj is not a cubebox")
		}
	}
	return cbs, nil
}

func (s *Store) GetCubeboxByTemplateID(templateID string) ([]*CubeBox, error) {
	return s.getCubeboxByIndex(templateIDIndexerKey, templateID)
}

func (s *Store) GetCubeboxByImageID(imageID string) ([]*CubeBox, error) {
	return s.getCubeboxByIndex(imageIDIndexerKey, imageID)
}

func (s *Store) GetCubeboxByBaseBlockID(blockID string) ([]*CubeBox, error) {
	return s.getCubeboxByIndex(baseBlockIDIndexerKey, blockID)
}
