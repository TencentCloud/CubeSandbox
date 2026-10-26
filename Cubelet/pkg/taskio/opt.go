// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package taskio

import "path"

type Option func()

type config struct {
	fifoDir string
}

const (
	fifoDir = "fifo"
)

var cfg = config{}

func FIFODir(dir string) Option {
	return func() {
		cfg.fifoDir = path.Join(dir, fifoDir)
	}
}
