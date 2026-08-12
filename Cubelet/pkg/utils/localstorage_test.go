// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/stretchr/testify/assert"
	bolt "go.etcd.io/bbolt"
)

func TestDeleteWithTxCallbackErrorRollsBack(t *testing.T) {
	db, err := NewCubeStore(filepath.Join(t.TempDir(), "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Set("bucket", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	want := errors.New("callback failed")
	if err := db.DeleteWithTx("bucket", "key", func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("DeleteWithTx error = %v, want %v", err, want)
	}
	if got, err := db.Get("bucket", "key"); err != nil || string(got) != "value" {
		t.Fatalf("deleted value after callback failure: %q, %v", got, err)
	}
}

// TestDeleteWithTxCallbackSuccessCommits is the positive counterpart: when the
// callback succeeds the delete must commit and DeleteWithTx must return nil.
// This guards against a regression where propagating the callback error would
// also surface a spurious error on the happy path.
func TestDeleteWithTxCallbackSuccessCommits(t *testing.T) {
	db, err := NewCubeStore(filepath.Join(t.TempDir(), "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Set("bucket", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}

	called := false
	if err := db.DeleteWithTx("bucket", "key", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("DeleteWithTx returned error on success: %v", err)
	}
	if !called {
		t.Fatal("callback was not invoked")
	}
	if _, err := db.Get("bucket", "key"); !errors.Is(err, ErrorKeyNotFound) {
		t.Fatalf("key still present after successful delete: err=%v", err)
	}
}

// TestDeleteWithTxNilCallbackDeletes covers the plain-delete path (nil callback,
// used by Delete): the key is removed and no error is returned.
func TestDeleteWithTxNilCallbackDeletes(t *testing.T) {
	db, err := NewCubeStore(filepath.Join(t.TempDir(), "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Set("bucket", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteWithTx("bucket", "key", nil); err != nil {
		t.Fatalf("DeleteWithTx(nil) returned error: %v", err)
	}
	if _, err := db.Get("bucket", "key"); !errors.Is(err, ErrorKeyNotFound) {
		t.Fatalf("key still present after nil-callback delete: err=%v", err)
	}
}

// TestDeleteWithTxBucketNotFoundSkipsCallback covers the early-return path: when
// the bucket does not exist the transaction aborts with ErrorBucketNotFound
// before the callback runs, so cascading-cleanup callbacks must not fire.
func TestDeleteWithTxBucketNotFoundSkipsCallback(t *testing.T) {
	db, err := NewCubeStore(filepath.Join(t.TempDir(), "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	called := false
	err = db.DeleteWithTx("missing", "key", func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrorBucketNotFound) {
		t.Fatalf("DeleteWithTx error = %v, want %v", err, ErrorBucketNotFound)
	}
	if called {
		t.Fatal("callback ran despite missing bucket")
	}
}

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
		value, _ := jsoniter.Marshal(vv)

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
		_ = jsoniter.Unmarshal(v, m)

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
		value, _ := jsoniter.Marshal(vv)
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
			_ = jsoniter.Unmarshal(v, m)

		}
	}
}
