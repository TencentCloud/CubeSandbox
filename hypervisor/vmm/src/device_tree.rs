// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::device_manager::PciDeviceHandle;
use pci::PciBdf;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::os::unix::io::RawFd;
use std::sync::{Arc, Mutex};
use virtio_devices::fs::FsEvent;
use vm_device::Resource;
use vm_migration::Migratable;

#[derive(Clone, Serialize, Deserialize)]
pub struct DeviceNode {
    pub id: String,
    pub resources: Vec<Resource>,
    pub parent: Option<String>,
    pub children: Vec<String>,
    #[serde(skip)]
    pub migratable: Option<Arc<Mutex<dyn Migratable>>>,
    pub pci_bdf: Option<PciBdf>,
    #[serde(skip)]
    pub pci_device_handle: Option<PciDeviceHandle>,
    #[serde(skip)]
    pub dev_fd: Option<RawFd>,
    #[serde(skip)]
    pub pending_dev_message: Option<Arc<Mutex<Vec<FsEvent>>>>,
}

impl DeviceNode {
    pub fn new(
        id: String,
        migratable: Option<Arc<Mutex<dyn Migratable>>>,
        dev_fd: Option<RawFd>,
        pending_dev_message: Option<Arc<Mutex<Vec<FsEvent>>>>,
    ) -> Self {
        DeviceNode {
            id,
            resources: Vec::new(),
            parent: None,
            children: Vec::new(),
            migratable,
            pci_bdf: None,
            pci_device_handle: None,
            dev_fd,
            pending_dev_message,
        }
    }
}

#[macro_export]
macro_rules! device_node {
    ($id:ident) => {
        DeviceNode::new($id.clone(), None, None, None)
    };
    ($id:ident, $device:ident) => {
        DeviceNode::new(
            $id.clone(),
            Some(Arc::clone(&$device) as Arc<Mutex<dyn Migratable>>),
            None,
            None,
        )
    };
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct DeviceTree(HashMap<String, DeviceNode>);

impl DeviceTree {
    pub fn new() -> Self {
        DeviceTree(HashMap::new())
    }
    pub fn contains_key(&self, k: &str) -> bool {
        self.0.contains_key(k)
    }
    pub fn get(&self, k: &str) -> Option<&DeviceNode> {
        self.0.get(k)
    }
    pub fn get_mut(&mut self, k: &str) -> Option<&mut DeviceNode> {
        self.0.get_mut(k)
    }
    pub fn insert(&mut self, k: String, v: DeviceNode) -> Option<DeviceNode> {
        self.0.insert(k, v)
    }
    pub fn remove(&mut self, k: &str) -> Option<DeviceNode> {
        self.0.remove(k)
    }
    pub fn iter(&self) -> std::collections::hash_map::Iter<String, DeviceNode> {
        self.0.iter()
    }
    pub fn breadth_first_traversal(&self) -> BftIter {
        BftIter::new(&self.0)
    }
    pub fn breadth_first_traversal_parent(&self) -> BftIter {
        BftIter::new_parent(&self.0)
    }
    pub fn breadth_first_traversal_children(&self) -> BftIter {
        BftIter::new_children(&self.0)
    }
    pub fn pci_devices(&self) -> Vec<&DeviceNode> {
        self.0
            .values()
            .filter(|v| v.pci_bdf.is_some() && v.pci_device_handle.is_some())
            .collect()
    }

    pub fn remove_node_by_pci_bdf(&mut self, pci_bdf: PciBdf) -> Option<DeviceNode> {
        let mut id = None;
        for (k, v) in self.0.iter() {
            if v.pci_bdf == Some(pci_bdf) {
                id = Some(k.clone());
                break;
            }
        }

        if let Some(id) = &id {
            self.0.remove(id)
        } else {
            None
        }
    }
}

// Breadth first traversal iterator.
pub struct BftIter<'a> {
    nodes: Vec<&'a DeviceNode>,
}

impl<'a> BftIter<'a> {
    fn new(hash_map: &'a HashMap<String, DeviceNode>) -> Self {
        let mut nodes = Vec::new();

        for (_, node) in hash_map.iter() {
            if node.parent.is_none() {
                nodes.push(node);
            }
        }

        nodes.sort_by(|a, b| a.id.cmp(&b.id));

        let mut node_layer = nodes.as_slice();
        loop {
            let mut next_node_layer = Vec::new();

            for node in node_layer.iter() {
                for child_node_id in node.children.iter() {
                    if let Some(child_node) = hash_map.get(child_node_id) {
                        next_node_layer.push(child_node);
                    }
                }
            }

            if next_node_layer.is_empty() {
                break;
            }

            next_node_layer.sort_by(|a, b| a.id.cmp(&b.id));

            let pos = nodes.len();
            nodes.extend(next_node_layer);

            node_layer = &nodes[pos..];
        }

        BftIter { nodes }
    }

    fn new_parent(hash_map: &'a HashMap<String, DeviceNode>) -> Self {
        let mut nodes = Vec::new();

        for (_, node) in hash_map.iter() {
            if node.parent.is_none() {
                nodes.push(node);
            }
        }

        nodes.sort_by(|a, b| a.id.cmp(&b.id));

        BftIter { nodes }
    }

    fn new_children(hash_map: &'a HashMap<String, DeviceNode>) -> Self {
        let mut nodes = Vec::new();
        let mut nodes_children = Vec::new();

        for (_, node) in hash_map.iter() {
            if node.parent.is_none() {
                nodes.push(node);
            }
        }

        nodes.sort_by(|a, b| a.id.cmp(&b.id));

        let mut node_layer = nodes.as_slice();
        loop {
            let mut next_node_layer = Vec::new();

            for node in node_layer.iter() {
                for child_node_id in node.children.iter() {
                    if let Some(child_node) = hash_map.get(child_node_id) {
                        next_node_layer.push(child_node);
                    }
                }
            }

            if next_node_layer.is_empty() {
                break;
            }

            next_node_layer.sort_by(|a, b| a.id.cmp(&b.id));

            let pos = nodes.len();
            nodes_children.extend(next_node_layer);

            node_layer = &nodes[pos..];
        }

        BftIter {
            nodes: nodes_children,
        }
    }
}

impl<'a> Iterator for BftIter<'a> {
    type Item = &'a DeviceNode;

    fn next(&mut self) -> Option<Self::Item> {
        if self.nodes.is_empty() {
            None
        } else {
            Some(self.nodes.remove(0))
        }
    }
}

impl<'a> DoubleEndedIterator for BftIter<'a> {
    fn next_back(&mut self) -> Option<Self::Item> {
        self.nodes.pop()
    }
}

#[cfg(test)]
mod tests {
    use super::{DeviceNode, DeviceTree};

    #[test]
    fn test_device_tree() {
        // Check new()
        let mut device_tree = DeviceTree::new();
        assert_eq!(device_tree.0.len(), 0);

        // Check insert()
        let id = String::from("id1");
        device_tree.insert(id.clone(), DeviceNode::new(id.clone(), None, None, None));
        assert_eq!(device_tree.0.len(), 1);
        let node = device_tree.0.get(&id);
        assert!(node.is_some());
        let node = node.unwrap();
        assert_eq!(node.id, id);

        // Check get()
        let id2 = String::from("id2");
        assert!(device_tree.get(&id).is_some());
        assert!(device_tree.get(&id2).is_none());

        // Check get_mut()
        let node = device_tree.get_mut(&id).unwrap();
        node.id = id2.clone();
        let node = device_tree.0.get(&id).unwrap();
        assert_eq!(node.id, id2);

        // Check remove()
        let node = device_tree.remove(&id).unwrap();
        assert_eq!(node.id, id2);
        assert_eq!(device_tree.0.len(), 0);

        // Check iter()
        let disk_id = String::from("disk0");
        let net_id = String::from("net0");
        let rng_id = String::from("rng0");
        let device_list = vec![
            (disk_id.clone(), device_node!(disk_id)),
            (net_id.clone(), device_node!(net_id)),
            (rng_id.clone(), device_node!(rng_id)),
        ];
        device_tree.0.extend(device_list);
        for (id, node) in device_tree.iter() {
            if id == &disk_id {
                assert_eq!(node.id, disk_id);
            } else if id == &net_id {
                assert_eq!(node.id, net_id);
            } else if id == &rng_id {
                assert_eq!(node.id, rng_id);
            } else {
                unreachable!()
            }
        }

        // Check breadth_first_traversal() based on the following hierarchy
        //
        // 0
        // | \
        // 1  2
        // |  | \
        // 3  4  5
        //
        let mut device_tree = DeviceTree::new();
        let child_1_id = String::from("child1");
        let child_2_id = String::from("child2");
        let child_3_id = String::from("child3");
        let parent_1_id = String::from("parent1");
        let parent_2_id = String::from("parent2");
        let root_id = String::from("root");
        let mut child_1_node = device_node!(child_1_id);
        let mut child_2_node = device_node!(child_2_id);
        let mut child_3_node = device_node!(child_3_id);
        let mut parent_1_node = device_node!(parent_1_id);
        let mut parent_2_node = device_node!(parent_2_id);
        let mut root_node = device_node!(root_id);
        child_1_node.parent = Some(parent_1_id.clone());
        child_2_node.parent = Some(parent_2_id.clone());
        child_3_node.parent = Some(parent_2_id.clone());
        parent_1_node.children = vec![child_1_id.clone()];
        parent_1_node.parent = Some(root_id.clone());
        parent_2_node.children = vec![child_2_id.clone(), child_3_id.clone()];
        parent_2_node.parent = Some(root_id.clone());
        root_node.children = vec![parent_1_id.clone(), parent_2_id.clone()];
        let device_list = vec![
            (child_1_id.clone(), child_1_node),
            (child_2_id.clone(), child_2_node),
            (child_3_id.clone(), child_3_node),
            (parent_1_id.clone(), parent_1_node),
            (parent_2_id.clone(), parent_2_node),
            (root_id.clone(), root_node),
        ];
        device_tree.0.extend(device_list);

        let iter_vec = device_tree
            .breadth_first_traversal()
            .collect::<Vec<&DeviceNode>>();
        assert_eq!(iter_vec.len(), 6);
        assert_eq!(iter_vec[0].id, root_id);
        assert_eq!(iter_vec[1].id, parent_1_id);
        assert_eq!(iter_vec[2].id, parent_2_id);
        assert_eq!(iter_vec[3].id, child_1_id);
        assert_eq!(iter_vec[4].id, child_2_id);
        assert_eq!(iter_vec[5].id, child_3_id);

        let iter_vec = device_tree
            .breadth_first_traversal()
            .rev()
            .collect::<Vec<&DeviceNode>>();
        assert_eq!(iter_vec.len(), 6);
        assert_eq!(iter_vec[5].id, root_id);
        assert_eq!(iter_vec[4].id, parent_1_id);
        assert_eq!(iter_vec[3].id, parent_2_id);
        assert_eq!(iter_vec[2].id, child_1_id);
        assert_eq!(iter_vec[1].id, child_2_id);
        assert_eq!(iter_vec[0].id, child_3_id);
    }
}
