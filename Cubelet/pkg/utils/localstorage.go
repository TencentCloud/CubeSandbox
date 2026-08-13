// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/containerd/errdefs"
	"github.com/hashicorp/go-multierror"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrorKeyNotFound    = fmt.Errorf("key %w", errdefs.ErrNotFound)
	ErrorBucketNotFound = fmt.Errorf("bucket %w", errdefs.ErrNotFound)
)

type CubeStore struct {
	poolNum  uint64
	indexMap map[uint64]*bolt.DB
	db       *bolt.DB
	closed   int32
}

func NewCubeStoreExt(path string, file string, num uint64, opts *bolt.Options) (*CubeStore, error) {
	if opts == nil {
		opts = MakeBoltDBOption()
	}
	if file == "" {
		file = "meta.db"
	}
	cs := &CubeStore{
		poolNum:  num,
		indexMap: map[uint64]*bolt.DB{},
	}
	begin := 'b'
	for i := uint64(0); i < num; i++ {
		filePath := filepath.Join(path, fmt.Sprintf("%c", begin))
		if err := os.MkdirAll(filePath, os.ModeDir|0755); err != nil {
			return nil, fmt.Errorf("mkdir failed %s", err.Error())
		}
		dbfile := filepath.Join(filePath, file)
		db, err := bolt.Open(dbfile, 0644, opts)
		if err != nil {
			return nil, fmt.Errorf("open bolt db %s failed: %s", dbfile, err.Error())
		}
		cs.indexMap[i] = db
		begin += 1
	}
	return cs, nil
}

func NewCubeStore(path string, opts *bolt.Options) (*CubeStore, error) {
	if opts == nil {
		opts = MakeBoltDBOption()
	}
	db, err := bolt.Open(path, 0644, opts)
	if err != nil {
		return nil, err
	}
	return &CubeStore{db: db}, nil
}

func (cs *CubeStore) Close() error {
	var (
		result *multierror.Error
	)
	atomic.StoreInt32(&cs.closed, 1)
	if cs.db != nil {
		if err := cs.db.Close(); err != nil {
			result = multierror.Append(result, err)
		}
	}
	if cs.poolNum > 0 {
		for _, v := range cs.indexMap {
			if err := v.Close(); err != nil {
				result = multierror.Append(result, err)
			}
		}
	}
	return result.ErrorOrNil()
}

func (cs *CubeStore) isClosed() bool {
	return atomic.LoadInt32(&cs.closed) == 1
}

func (cs *CubeStore) Get(bucket, key string) (result []byte, err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		real_index := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (real_index%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {

			return ErrorBucketNotFound
		}
		v := b.Get([]byte(key))

		if v == nil {

			return ErrorKeyNotFound
		}
		result = append(result, v...)
		return nil
	})
	return
}

func (cs *CubeStore) Set(bucket, key string, value []byte) (err error) {
	return cs.SetWithTx(bucket, key, value, nil)
}

func (cs *CubeStore) SetWithTx(bucket, key string, value []byte, callback func() error) (err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		realIndex := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (realIndex%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		err = b.Put([]byte(key), value)
		if err != nil {
			return err
		}
		if callback != nil {
			err = callback()
		}
		return err
	})
	return
}

func (cs *CubeStore) Delete(bucket, key string) (err error) {
	return cs.DeleteWithTx(bucket, key, nil)
}

func (cs *CubeStore) DeleteWithTx(bucket, key string, callback func() error) (err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		realIndex := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (realIndex%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return ErrorBucketNotFound
		}

		err := b.Delete([]byte(key))
		if err != nil {
			return err
		}
		if callback != nil {
			err = callback()
		}
		return err
	})
	return
}

func (cs *CubeStore) ReadAll(bucket string) (all map[string][]byte, _ error) {
	if cs.poolNum > 0 {
		var (
			result *multierror.Error
		)
		all = make(map[string][]byte)
		for _, v := range cs.indexMap {
			err := v.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(bucket))
				if b == nil {
					return nil
				}
				if err := b.ForEach(func(k, v []byte) error {
					b := make([]byte, 0, len(v))
					all[string(k)] = append(b, v...)
					return nil
				}); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				result = multierror.Append(result, err)
			}
		}
		return all, result.ErrorOrNil()
	} else {
		err := cs.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(bucket))
			if b == nil {
				return nil
			}
			all = map[string][]byte{}
			if err := b.ForEach(func(k, v []byte) error {
				b := make([]byte, 0, len(v))
				all[string(k)] = append(b, v...)
				return nil
			}); err != nil {
				return err
			}
			return nil
		})
		return all, err
	}
}

func (cs *CubeStore) SetBs(key string, value []byte, buckets ...[]byte) (err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		realIndex := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (realIndex%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := createBucketIfNotExists(tx, buckets...)
		if err != nil {
			return err
		}
		err = b.Put([]byte(key), value)
		return err
	})
	return
}

func (cs *CubeStore) GetBs(key string, buckets ...[]byte) (result []byte, err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		realIndex := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (realIndex%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.View(func(tx *bolt.Tx) error {
		b := getBucket(tx, buckets...)
		if b == nil {

			return ErrorBucketNotFound
		}
		v := b.Get([]byte(key))

		if v == nil {

			return ErrorKeyNotFound
		}
		result = append(result, v...)
		return nil
	})
	return
}

func (cs *CubeStore) DeleteBs(key string, buckets ...[]byte) (err error) {
	var db *bolt.DB
	if cs.poolNum > 0 {
		realIndex := uint64(crc32.ChecksumIEEE([]byte(key)))
		index := (realIndex%cs.poolNum + cs.poolNum) % cs.poolNum
		db = cs.indexMap[index]
	} else {
		db = cs.db
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b := getBucket(tx, buckets...)
		if b == nil {
			return ErrorBucketNotFound
		}
		err := b.Delete([]byte(key))

		if err != nil {
			return err
		}
		return nil
	})
	return
}

func (cs *CubeStore) ReadAllBs(buckets ...[]byte) (all map[string][]byte, _ error) {
	if cs.poolNum > 0 {
		var (
			result *multierror.Error
		)
		all = map[string][]byte{}
		for _, v := range cs.indexMap {
			if cs.isClosed() {
				break
			}
			err := v.View(func(tx *bolt.Tx) error {
				b := getBucket(tx, buckets...)
				if b == nil {
					return nil
				}
				if err := b.ForEach(func(k, v []byte) error {
					b := make([]byte, 0, len(v))
					all[string(k)] = append(b, v...)
					return nil
				}); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				result = multierror.Append(result, err)
			}
		}
		return all, result.ErrorOrNil()
	} else {
		err := cs.db.View(func(tx *bolt.Tx) error {
			b := getBucket(tx, buckets...)
			if b == nil {
				return nil
			}
			all = map[string][]byte{}
			if err := b.ForEach(func(k, v []byte) error {
				b := make([]byte, 0, len(v))
				all[string(k)] = append(b, v...)
				return nil
			}); err != nil {
				return err
			}
			return nil
		})
		return all, err
	}
}

func getBucket(tx *bolt.Tx, keys ...[]byte) *bolt.Bucket {
	bkt := tx.Bucket(keys[0])

	for _, key := range keys[1:] {
		if bkt == nil {
			break
		}
		bkt = bkt.Bucket(key)
	}

	return bkt
}

func createBucketIfNotExists(tx *bolt.Tx, keys ...[]byte) (*bolt.Bucket, error) {
	bkt, err := tx.CreateBucketIfNotExists(keys[0])
	if err != nil {
		return nil, err
	}

	for _, key := range keys[1:] {
		bkt, err = bkt.CreateBucketIfNotExists(key)
		if err != nil {
			return nil, err
		}
	}

	return bkt, nil
}
