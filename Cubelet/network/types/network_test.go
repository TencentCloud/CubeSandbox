// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"encoding/json"
	"strings"
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

func TestDecodeNetRequestValidatesCanonicalQos(t *testing.T) {
	req, err := DecodeNetRequest(`{"Qos":{"BandWidth":{"Size":1250000,"OneTimeBurst":0,"RefillTime":100},"OPS":{"Size":5000,"OneTimeBurst":0,"RefillTime":1000}},"Version":1}`)
	if err != nil {
		t.Fatalf("DecodeNetRequest error=%v", err)
	}
	if req.Qos == nil || req.Qos.BandWidth.Size != 1250000 || req.Qos.OPS.Size != 5000 || req.Qos.OPS.RefillTime != 1000 {
		t.Fatalf("request=%+v", req)
	}
}

func TestDecodeNetRequestAcceptsPacketsOnlyQos(t *testing.T) {
	req, err := DecodeNetRequest(`{"Qos":{"BandWidth":{},"OPS":{"Size":5000,"RefillTime":1000}},"Version":1}`)
	if err != nil {
		t.Fatalf("DecodeNetRequest error=%v", err)
	}
	if req.Qos == nil || req.Qos.BandWidth.Size != 0 || req.Qos.OPS.Size != 5000 {
		t.Fatalf("request=%+v", req)
	}
}

func TestDecodeNetRequestAcceptsLegacyVersionZero(t *testing.T) {
	_, err := DecodeNetRequest(`{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}}}`)
	if err != nil {
		t.Fatalf("DecodeNetRequest error=%v", err)
	}
}

func TestDecodeNetRequestPreservesLegacyNoOpPayloads(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"Mode":"legacy","Extension":true}`,
		`{"Qos":{"Bandwidth":{"Size":1250000,"RefillTime":100}}}`,
	} {
		if _, err := DecodeNetRequest(raw); err != nil {
			t.Fatalf("DecodeNetRequest(%s) error=%v", raw, err)
		}
	}
}

func TestDecodeNetRequestRejectsUnknownOrMisCasedFields(t *testing.T) {
	_, err := DecodeNetRequest(`{"Qos":{"Bandwidth":{"Size":1250000,"RefillTime":100}},"Version":1}`)
	if err == nil || !strings.Contains(err.Error(), "Bandwidth") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecodeNetRequestRejectsInvalidBuckets(t *testing.T) {
	tests := []string{
		`{"Qos":{"BandWidth":{"Size":-1,"RefillTime":100}},"Version":1}`,
		`{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":0}},"Version":1}`,
		`{"Qos":{"BandWidth":{},"OPS":{"Size":5000,"RefillTime":0}},"Version":1}`,
		`{"Qos":{"BandWidth":{},"OPS":{}},"Version":1}`,
	}
	for _, raw := range tests {
		if _, err := DecodeNetRequest(raw); err == nil {
			t.Fatalf("expected invalid bucket error for %s", raw)
		}
	}
}

func TestDecodeNetRequestRejectsUnsupportedVersionAndMode(t *testing.T) {
	for _, raw := range []string{
		`{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}},"Version":2}`,
		`{"Mode":"legacy","Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}},"Version":1}`,
	} {
		if _, err := DecodeNetRequest(raw); err == nil {
			t.Fatalf("expected request rejection for %s", raw)
		}
	}
}
