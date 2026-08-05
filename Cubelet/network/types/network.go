// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"context"
	"encoding/json"
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
	Mode    string
	Qos     *NetQosConfig
	Version uint64
}

type NetQosConfig struct {
	BandWidth LimiterConfig
	OPS       LimiterConfig
}

type LimiterConfig struct {
	Size         int
	OneTimeBurst int
	RefillTime   int
}

type PortMapping struct {
	HostPort      uint16
	ContainerPort uint16
}
