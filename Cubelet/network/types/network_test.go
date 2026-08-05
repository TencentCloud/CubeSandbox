// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestNetworkRequest(t *testing.T) {
	req := NetRequest{
		Qos: &NetQosConfig{
			BandWidth: LimiterConfig{
				Size:         1200000,
				OneTimeBurst: 12000,
				RefillTime:   1000,
			},
			OPS: LimiterConfig{
				Size:         12000,
				OneTimeBurst: 120,
				RefillTime:   1000,
			},
		},
	}
	data, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}

	data, err = json.Marshal(map[string]string{
		constants.MasterAnnotationsNetWork: string(data),
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Log(string(data))
}
