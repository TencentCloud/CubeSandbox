// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controller

type CubeMetaController interface {
	Run(stopCh <-chan struct{}) error
}
