// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::{Deserialize, Serialize};
pub const ANNO_NET: &str = "cube.net";
pub const CH_NET_ID: &str = "tap-0";

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Net {
    pub interfaces: Vec<Interface>,
    pub routes: Vec<Route>,
    pub arps: Vec<Arp>,
}

impl Net {
    pub fn get_pb_interfaces(&self) -> Vec<protoc::types::Interface> {
        let mut nets = Vec::new();
        for net in self.interfaces.iter() {
            let mut ips: Vec<protoc::types::IPAddress> = Vec::new();

            if net.ips.len() == 0 {
                // 待废弃（等cubelet发完turbocfs的功能之后，net的标记为deprecated的字段就可以删除了）
                let mut family = protoc::types::IPFamily::v4;
                if family as u32 != net.family {
                    family = protoc::types::IPFamily::v6;
                }
                let ip = protoc::types::IPAddress {
                    family,
                    address: net.ip.clone(),
                    mask: net.mask.to_string(),
                    ..Default::default()
                };

                ips.push(ip);
            } else {
                ips = Vec::with_capacity(net.ips.len());
                for ip in net.ips.iter() {
                    ips.push(ip.into());
                }
            }

            let n = protoc::types::Interface {
                name: net.guest_name.clone(),
                mtu: net.mtu as u64,
                hwAddr: net.mac.clone(),
                IPAddresses: ips.into(),
                ..Default::default()
            };

            nets.push(n);
        }
        nets
    }
    /*
           Dest:    r.Dest,
           Gateway: r.Gateway,
           Device:  r.Device,
           Source:  r.Source,
           Scope:   uint32(r.Scope),
           Family:  pbTypes.IPFamily(r.Family),
    */
    pub fn get_pb_routes(&self) -> Vec<protoc::types::Route> {
        let mut routes = Vec::new();
        for route in self.routes.iter() {
            let mut family = protoc::types::IPFamily::v4;
            if family as u32 != route.family {
                family = protoc::types::IPFamily::v6;
            }
            let r = protoc::types::Route {
                dest: route.dest.clone(),
                gateway: route.gateway.clone(),
                device: route.device.clone(),
                source: route.source.clone(),
                scope: route.scope,
                onlink: route.onlink,
                family,
                ..Default::default()
            };
            routes.push(r);
        }
        routes
    }

    pub fn get_pb_arps(&self) -> Vec<protoc::types::ARPNeighbor> {
        let mut arps = Vec::new();
        for arp in self.arps.iter() {
            let addr = protoc::types::IPAddress {
                address: arp.dest_ip.clone(),
                ..Default::default()
            };
            let a = protoc::types::ARPNeighbor {
                toIPAddress: Some(addr).into(),
                device: arp.device.clone(),
                lladdr: arp.ll_addr.clone(),
                flags: arp.flags as i32,
                state: arp.state as i32,
                ..Default::default()
            };
            arps.push(a);
        }
        arps
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Interface {
    /*
    type Interface struct {
        GuestBdf           string
    }

     */
    pub name: Option<String>,
    pub guest_name: String,
    pub mac: String,
    pub mtu: u32,
    //#[deprecated = "ip字段是一个过时的字段，仅当ips字段为空时，才使用这个字段的值。"]
    pub ip: String,
    //#[deprecated = "family字段是一个过时的字段，仅当ips字段为空时，才使用这个字段的值。"]
    pub family: u32,
    //#[deprecated = " mask字段是一个过时的字段，仅当ips字段为空时，才使用这个字段的值。"]
    pub mask: u32,
    #[serde(default)]
    pub ips: Vec<MVMIp>,
    pub qos: Option<NetQos>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct MVMIp {
    pub ip: String,
    pub family: u32,
    pub mask: u32,
}

impl Into<protoc::types::IPAddress> for &MVMIp {
    fn into(self) -> protoc::types::IPAddress {
        let mut family = protoc::types::IPFamily::v4;
        if family as u32 != self.family {
            family = protoc::types::IPFamily::v6;
        }
        let ip = protoc::types::IPAddress {
            family,
            address: self.ip.clone(),
            mask: self.mask.to_string(),
            ..Default::default()
        };

        ip
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Route {
    pub family: u32,
    pub dest: String,
    pub gateway: String,
    pub source: String,
    pub device: String,
    pub scope: u32,
    #[serde(default)]
    pub onlink: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Arp {
    pub dest_ip: String,
    pub device: String,
    pub ll_addr: String,
    pub state: u32,
    pub flags: u32,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct NetQos {
    pub bw_size: u64,
    pub bw_one_time_burst: u64,
    pub bw_refill_time: u64,
    pub ops_size: u64,
    pub ops_one_time_burst: u64,
    pub ops_refill_time: u64,
}
