// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi"
)

func Test_cubeHostImageReverseClient_Run(t *testing.T) {
	type fields struct {
		ns        string
		factory   *sandboxVSocketFactory
		closeChan chan struct{}
		errChan   chan error
		updater   chi.CubeboxRuntimeUpdater
		vmclient  cubehost.CubeHostImageServiceClient
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &cubeHostImageReverseClient{
				ns:        tt.fields.ns,
				factory:   tt.fields.factory,
				closeChan: tt.fields.closeChan,
				errChan:   tt.fields.errChan,
				updater:   tt.fields.updater,
				vmClient:  tt.fields.vmclient,
			}
			s.ID()

		})
	}
}

func Test_cubeHostImageReverseClient_TwoCloseChan(t *testing.T) {
	closeChan := make(chan struct{})
	close(closeChan)
	<-closeChan
	t.Log("test 1")
	<-closeChan
	t.Log("test 2")
}
