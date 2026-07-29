// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/containerd/containerd/v2/pkg/oci"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

type QosConfig struct {
	BwSize          int `json:"bw_size"`
	BwOneTimeBurst  int `json:"bw_one_time_burst"`
	BwRefillTime    int `json:"bw_refill_time"`
	OpsSize         int `json:"ops_size"`
	OpsOneTimeBurst int `json:"ops_one_time_burst"`
	OpsRefillTime   int `json:"ops_refill_time"`
}

type Interface struct {
	Name      string `json:"name"`
	IPAddr    net.IP `json:"-"`
	GuestName string `json:"guest_name"`
	Mac       string `json:"mac"`
	Mtu       int    `json:"mtu"`

	IP string `json:"ip"`

	Family int `json:"family"`

	Mask int        `json:"mask"`
	IPs  []MVMIp    `json:"ips"`
	Qos  *QosConfig `json:"qos"`
}

type MVMIp struct {
	IP     string `json:"ip"`
	Family int    `json:"family"`
	Mask   int    `json:"mask"`
}

type Route struct {
	Family  int    `json:"family"`
	Dest    string `json:"dest"`
	Gateway string `json:"gateway"`
	Source  string `json:"source"`
	Device  string `json:"device"`
	Scope   int    `json:"scope"`
	Onlink  bool   `json:"onlink"`
}

type ARP struct {
	DestIP string `json:"dest_ip"`
	Device string `json:"device"`
	LlAddr string `json:"ll_addr"`
	State  int    `json:"state"`
	Flags  int    `json:"flags"`
}

type ShimNetReqPersistMetadata struct {
	SandboxIP string `json:"sandbox_ip"`
}

type ShimNetReq struct {
	Interfaces   []*Interface  `json:"interfaces"`
	Routes       []Route       `json:"routes"`
	ARPs         []ARP         `json:"arps"`
	PortMappings []PortMapping `json:"port_mappings"`
}

func (r *ShimNetReq) GetPersistMetadata() []byte {
	md := ShimNetReqPersistMetadata{
		SandboxIP: r.SandboxIP(),
	}
	b, e := json.Marshal(md)
	if e != nil {
		log.G(context.Background()).Errorf("failed to marshal ShimNetReq persist metadata, err: %v", e)
		return nil
	}

	return b
}

func (r *ShimNetReq) SandboxIP() string {
	if len(r.Interfaces) <= 0 {
		return ""
	}
	return r.Interfaces[0].IPAddr.String()
}

func (r *ShimNetReq) GatewayIP() string {
	if len(r.Routes) <= 0 {
		return ""
	}
	return r.Routes[0].Gateway
}

func (r *ShimNetReq) AllocatedPorts() []PortMapping {
	return r.PortMappings
}

func (r *ShimNetReq) OCISpecOpts() oci.SpecOpts {
	b, _ := jsoniter.Marshal(r)

	return oci.WithAnnotations(map[string]string{
		constants.AnnotationsNetWork: string(b),
	})
}

type NetRequest struct {
	Mode    string        `json:"Mode,omitempty"`
	Qos     *NetQosConfig `json:"Qos"`
	Version uint64        `json:"Version,omitempty"`
}

func (req *NetRequest) Validate() error {
	if req == nil {
		return errors.New("network request is nil")
	}
	if req.Version > 1 {
		return fmt.Errorf("unsupported network request version %d", req.Version)
	}
	if req.Version == 1 && req.Mode != "" {
		return fmt.Errorf("unsupported network request mode %q", req.Mode)
	}
	if req.Qos == nil {
		if req.Version == 0 {
			return nil
		}
		return errors.New("network request version 1 requires Qos")
	}
	bandwidthEnabled, err := req.Qos.BandWidth.validate("Qos.BandWidth")
	if err != nil {
		return err
	}
	opsEnabled, err := req.Qos.OPS.validate("Qos.OPS")
	if err != nil {
		return err
	}
	if req.Version == 1 && !bandwidthEnabled && !opsEnabled {
		return errors.New("network request must enable BandWidth or OPS")
	}
	return nil
}

type NetQosConfig struct {
	BandWidth LimiterConfig `json:"BandWidth"`
	OPS       LimiterConfig `json:"OPS"`
}

type LimiterConfig struct {
	Size         int `json:"Size"`
	OneTimeBurst int `json:"OneTimeBurst"`
	RefillTime   int `json:"RefillTime"`
}

func (c LimiterConfig) validate(path string) (bool, error) {
	if c.Size < 0 || c.OneTimeBurst < 0 || c.RefillTime < 0 {
		return false, fmt.Errorf("%s values must not be negative", path)
	}
	if c.Size == 0 && c.OneTimeBurst == 0 && c.RefillTime == 0 {
		return false, nil
	}
	if c.Size == 0 || c.RefillTime == 0 {
		return false, fmt.Errorf("%s requires positive Size and RefillTime", path)
	}
	return true, nil
}

func DecodeNetRequest(raw string) (*NetRequest, error) {
	request := &NetRequest{}
	if err := json.Unmarshal([]byte(raw), request); err != nil {
		return nil, err
	}
	if request.Version == 0 {
		if err := request.Validate(); err != nil {
			return nil, err
		}
		return request, nil
	}
	if request.Version > 1 {
		return nil, fmt.Errorf("unsupported network request version %d", request.Version)
	}

	var top map[string]json.RawMessage
	if err := decodeStrictJSONObject([]byte(raw), &top, "Mode", "Qos", "Version"); err != nil {
		return nil, err
	}
	qosRaw, ok := top["Qos"]
	if !ok {
		return nil, errors.New("network request requires Qos")
	}
	var qosObject map[string]json.RawMessage
	if err := decodeStrictJSONObject(qosRaw, &qosObject, "BandWidth", "OPS"); err != nil {
		return nil, fmt.Errorf("decode Qos: %w", err)
	}
	for name, bucketRaw := range qosObject {
		var bucket map[string]json.RawMessage
		if err := decodeStrictJSONObject(bucketRaw, &bucket, "Size", "OneTimeBurst", "RefillTime"); err != nil {
			return nil, fmt.Errorf("decode Qos.%s: %w", name, err)
		}
	}
	request = &NetRequest{}
	if err := json.Unmarshal([]byte(raw), request); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return request, nil
}

func decodeStrictJSONObject(data []byte, out *map[string]json.RawMessage, allowed ...string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	allowedKeys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedKeys[key] = struct{}{}
	}
	for key := range *out {
		if _, ok := allowedKeys[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

type PortMapping struct {
	HostPort      uint16
	ContainerPort uint16
}
