// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"encoding/json"

	"github.com/stretchr/testify/assert"
	bolt "go.etcd.io/bbolt"
)

func TestSet_SingleDb(t *testing.T) {
	basePath := t.TempDir()
	file := filepath.Join(basePath, "meta.db")
	db, err := NewCubeStore(file, bolt.DefaultOptions)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	b1 := "code"
	key := "code"
	value := []byte("vvvv")
	err = db.Set(b1, key, value)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	got, err := db.Get(b1, key)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, value, got)

	err = db.Delete(b1, key)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	err = db.Delete(b1, key)
	assert.Equal(t, nil, err)

	for i := 0; i < 10; i++ {
		value := []byte(strconv.FormatInt(int64(i), 10))
		key := strconv.FormatInt(rand.Int63(), 10)
		err = db.Set(b1, key, value)
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}
	all, err := db.ReadAll(b1)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, 10, len(all))
	for k, v := range all {
		_, _ = k, v

	}
}

func TestSet_MultiDb(t *testing.T) {
	basePath := t.TempDir()
	db, err := NewCubeStoreExt(basePath, "meta.db", 10, bolt.DefaultOptions)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	b1 := "code"
	key := "code"
	value := []byte("vvvv")
	err = db.Set(b1, key, value)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	got, err := db.Get(b1, key)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, value, got)

	err = db.Delete(b1, key)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	err = db.Delete(b1, key)
	assert.Equal(t, nil, err)

	for i := 0; i < 10; i++ {
		value := []byte(strconv.FormatInt(int64(i), 10))
		key := strconv.FormatInt(rand.Int63(), 10)
		err = db.Set(b1, key, value)
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}
	all, err := db.ReadAll(b1)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, 10, len(all))
	for k, v := range all {
		_, _ = k, v

	}
}

func TestSetBs_SingleDb(t *testing.T) {
	basePath := t.TempDir()
	file := filepath.Join(basePath, "meta.db")
	db, err := NewCubeStore(file, bolt.DefaultOptions)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	b1 := []byte("code")
	b2 := []byte(fmt.Sprintf("%d", 123456))
	key := "code"
	value := []byte("vvvv")
	err = db.SetBs(key, value, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	got, err := db.GetBs(key, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, value, got)

	err = db.DeleteBs(key, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	err = db.DeleteBs(key, b1, b2)
	assert.Equal(t, nil, err)
	b2s := [][]byte{[]byte(fmt.Sprintf("%d", 123456)),
		[]byte(fmt.Sprintf("%d", 123457)),
		[]byte(fmt.Sprintf("%d", 123458))}
	for i := 0; i < 10; i++ {
		value := []byte(strconv.FormatInt(int64(i), 10))
		key := strconv.FormatInt(rand.Int63(), 10)
		err = db.SetBs(key, value, b1, b2s[rand.Int()%len(b2s)])
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}
	allBs, err := db.ReadAllBs(b1)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, 3, len(allBs))
	for b, _ := range allBs {
		all, err := db.ReadAllBs(b1, []byte(b))
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
		for k, v := range all {
			_, _ = k, v

		}
	}
}

func TestSetBs_Multidb(t *testing.T) {
	basePath := t.TempDir()
	db, err := NewCubeStoreExt(basePath, "meta.db", 10, bolt.DefaultOptions)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	b1 := []byte("code")
	b2 := []byte(fmt.Sprintf("%d", 123456))
	key := "code"
	value := []byte("vvvv")
	err = db.SetBs(key, value, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	got, err := db.GetBs(key, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, value, got)

	err = db.DeleteBs(key, b1, b2)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	err = db.DeleteBs(key, b1, b2)
	assert.Equal(t, nil, err)

	b2s := [][]byte{[]byte(fmt.Sprintf("%d", 123456)),
		[]byte(fmt.Sprintf("%d", 123457)),
		[]byte(fmt.Sprintf("%d", 123458))}
	for i := 0; i < 10; i++ {
		value := []byte(strconv.FormatInt(int64(i), 10))
		key := strconv.FormatInt(rand.Int63(), 10)
		err = db.SetBs(key, value, b1, b2s[rand.Int()%len(b2s)])
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}
	allBs, err := db.ReadAllBs(b1)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.LessOrEqual(t, len(allBs), len(b2s))
	for b, _ := range allBs {
		all, err := db.ReadAllBs(b1, []byte(b))
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
		for k, v := range all {
			_, _ = k, v

		}
	}
}

func TestSetBs_Multidb_SingelKey_BenchMark(t *testing.T) {
	SkipCI(t)

	basePath := t.TempDir()
	options := *bolt.DefaultOptions
	options.NoSync = true
	options.NoGrowSync = true

	db, err := NewCubeStoreExt(basePath, "meta.db", 10, &options)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()

	}()

	b1 := "code"
	num := 1000000
	type meta struct {
		Timestamp int64 `json:"timestamp"`
		FileSize  int64 `json:"fileSize"`
		Ref       int   `json:"ref"`
	}

	for i := 0; i < num; i++ {
		vv := &meta{
			Ref:       rand.Intn(10000),
			FileSize:  1024 * 1024 * 50,
			Timestamp: time.Now().Unix(),
		}
		value, _ := json.Marshal(vv)

		key := strconv.FormatInt(int64(i), 10) + strconv.FormatInt(int64(i), 10)
		err = db.Set(b1, key, value)
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}

	all, err := db.ReadAll(b1)
	assert.Equal(t, len(all), num)
	for _, v := range all {
		m := &meta{}
		_ = json.Unmarshal(v, m)

	}
}

func TestSetBs_Multidb_MultiKeys_BenchMark(t *testing.T) {
	SkipCI(t)

	basePath := t.TempDir()
	options := *bolt.DefaultOptions
	options.NoSync = true
	options.NoGrowSync = true

	db, err := NewCubeStoreExt(basePath, "meta.db", 10, &options)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()

	}()

	b1 := []byte("code")
	var b2s [][]byte
	num := 1000000
	for i := 0; i < num; i++ {
		b2s = append(b2s, []byte(strconv.FormatInt(int64(i), 10)))
	}
	type meta struct {
		Timestamp int64 `json:"timestamp"`
		FileSize  int64 `json:"fileSize"`
		Ref       int   `json:"ref"`
	}
	for i := 0; i < num; i++ {
		vv := &meta{
			Ref:       rand.Intn(10000),
			FileSize:  1024 * 1024 * 50,
			Timestamp: time.Now().Unix(),
		}
		value, _ := json.Marshal(vv)
		key := strconv.FormatInt(int64(i), 10)
		err = db.SetBs(key, value, b1, b2s[i])
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
	}

	allBs, err := db.ReadAll(string(b1))
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}
	assert.Equal(t, num, len(allBs))
	for b, _ := range allBs {
		all, err := db.ReadAllBs(b1, []byte(b))
		if err != nil {
			assert.Nil(t, err)
			t.FailNow()
		}
		for _, v := range all {
			m := &meta{}
			_ = json.Unmarshal(v, m)

		}
	}
}
