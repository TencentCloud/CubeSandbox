// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package taskio

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/fifo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// safeContainerDir validates the container name and returns the safe path
// under cfg.fifoDir, preventing path traversal attacks.
func safeContainerDir(container string) (string, error) {
	return utils.SafeJoinPath(cfg.fifoDir, container)
}

type taskIO struct {
	filename string
	file     io.ReadWriteCloser
}

func (t *taskIO) Config() cio.Config {
	return cio.Config{
		Stdout: t.filename,
		Stderr: t.filename,
	}
}

func (t *taskIO) Cancel() {
}

func (t *taskIO) Wait() {
}

func (t *taskIO) Close() error {
	return t.file.Close()
}

func Init(opts ...Option) {
	for _, opt := range opts {
		opt()
	}
	if err := os.MkdirAll(cfg.fifoDir, 0755); err != nil {
		panic(fmt.Errorf("fifo init failed.%s", err))
	}
}

func New(null bool) cio.Creator {

	if null {
		return func(id string) (cio.IO, error) {
			return cio.NullIO(id)
		}
	}

	return func(id string) (cio.IO, error) {
		filename, err := genFIFOFile(id)
		if err != nil {
			return nil, err
		}
		file, err := fifo.OpenFifo(context.Background(), filename, syscall.O_RDWR|syscall.O_CREAT, 0600)
		if err != nil {
			return nil, err
		}
		return &taskIO{
			filename: filename,
			file:     file,
		}, nil
	}
}

func genFIFOFile(container string) (string, error) {
	containerDir, err := safeContainerDir(container)
	if err != nil {
		return "", fmt.Errorf("genFIFOFile: %w", err)
	}
	if err := os.MkdirAll(containerDir, os.ModeDir|0755); err != nil {
		return "", err
	}
	return path.Join(containerDir, container), nil
}

func GetFIFOFile(container string) string {
	containerDir, err := safeContainerDir(container)
	if err != nil {
		return ""
	}
	return path.Join(containerDir, container)
}

func Clean(ctx context.Context, container string) error {
	if container == "" {
		return nil
	}
	containerDir, err := safeContainerDir(container)
	if err != nil {
		return fmt.Errorf("Clean: %w", err)
	}
	exist, err := utils.DenExist(containerDir)
	if err != nil {
		return err
	}

	if exist {
		return os.RemoveAll(containerDir)
	}
	return nil
}
