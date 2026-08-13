// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package taskio

import (
	"context"
	"os"
	"path"
	"syscall"
	"testing"

	"github.com/containerd/fifo"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	tmpDir = "./testdir/fifo"
)

func TestInit(t *testing.T) {
	Init(FIFODir(tmpDir))
	assert.Equal(t, path.Join(tmpDir, fifoDir), cfg.fifoDir)
	exist, err := utils.DenExist(cfg.fifoDir)
	assert.True(t, exist)
	assert.Nil(t, err)
	err = os.RemoveAll(cfg.fifoDir)
	assert.Nil(t, err)
}

func TestGetFIFOFile(t *testing.T) {
	Init(FIFODir(tmpDir))
	ctnID := uuid.NewString()
	logfile := GetFIFOFile(ctnID)
	assert.Equal(t, path.Join(cfg.fifoDir, ctnID, ctnID), logfile)
}

func TestGenFIFOFile(t *testing.T) {
	Init(FIFODir(tmpDir))
	ctnID := uuid.NewString()
	logfile, err := genFIFOFile(ctnID)
	assert.Nil(t, err)
	assert.Equal(t, path.Join(cfg.fifoDir, ctnID, ctnID), logfile)

	exist, err := utils.DenExist(path.Join(cfg.fifoDir, ctnID))
	assert.True(t, exist)
	assert.Nil(t, err)
	Clean(context.Background(), ctnID)
}

func TestTaskIO_null(t *testing.T) {
	Init(FIFODir(tmpDir))
	creator := New(true)
	assert.NotNil(t, creator)
	id := uuid.NewString()
	cio, err := creator(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cio.Cancel()
	cfg := cio.Config()
	assert.Empty(t, cfg.Stderr)
	assert.Empty(t, cfg.Stdin)
	assert.Empty(t, cfg.Stdout)

	Clean(context.Background(), "")
}

func TestTaskIO(t *testing.T) {
	Init(FIFODir(tmpDir))
	assert.Equal(t, path.Join(tmpDir, fifoDir), cfg.fifoDir)

	creator := New(false)
	ctnID := uuid.NewString()
	cio, err := creator(ctnID)
	if err != nil {
		t.Fatal(err)
	}
	defer cio.Cancel()

	cfg := cio.Config()
	if cfg.Stdout != cfg.Stderr {
		t.Fatal("stdout and stderr not equal")
	}
	if cfg.Stdout != GetFIFOFile(ctnID) {
		t.Fatal("wrong fifo path")
	}
	defer Clean(context.Background(), ctnID)

	expected := 10
	ch, err := Consume(cfg.Stdout)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		f, err := fifo.OpenFifo(context.Background(), cfg.Stdout, syscall.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		for i := 0; i < expected; i++ {
			f.Write([]byte("hello world\n"))
		}
		f.Close()

		cio.Wait()
		cio.Close()
	}()

	actualCnt := 0
	for range ch {
		actualCnt++

	}
	assert.Equal(t, expected, actualCnt)
}
