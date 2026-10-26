// Copyright (c) 2026 Cloud Hypervisor Authors
//
// SPDX-License-Identifier: Apache-2.0

//! Validates virtio descriptor buffers before a device accesses guest memory.

use std::ops::Deref;

use log::warn;
use virtio_queue::{Descriptor, DescriptorChain};
use vm_memory::{GuestAddress, GuestMemory};

use crate::AccessPlatform;

/// A descriptor whose complete buffer range has been validated.
#[derive(Debug)]
pub struct CheckedDescriptor {
    inner: Descriptor,
    addr: GuestAddress,
}

impl CheckedDescriptor {
    pub fn addr(&self) -> GuestAddress {
        self.addr
    }

    pub fn len(&self) -> u32 {
        self.inner.len()
    }

    pub fn is_write_only(&self) -> bool {
        self.inner.is_write_only()
    }
}

/// Iterator adapter that stops at the first descriptor whose translated
/// buffer is not fully backed by guest memory.
pub struct CheckedDescriptorIter<'a, M> {
    chain: &'a mut DescriptorChain<M>,
    access_platform: Option<&'a dyn AccessPlatform>,
    done: bool,
}

impl<'a, M> CheckedDescriptorIter<'a, M>
where
    M: Deref,
    M::Target: GuestMemory,
{
    fn new(
        chain: &'a mut DescriptorChain<M>,
        access_platform: Option<&'a dyn AccessPlatform>,
    ) -> Self {
        Self {
            chain,
            access_platform,
            done: false,
        }
    }
}

impl<M> Iterator for CheckedDescriptorIter<'_, M>
where
    M: Deref,
    M::Target: GuestMemory,
{
    type Item = Result<CheckedDescriptor, GuestAddress>;

    fn next(&mut self) -> Option<Self::Item> {
        if self.done {
            return None;
        }

        let desc = self.chain.next()?;
        if desc.len() == 0 {
            return Some(Ok(CheckedDescriptor {
                addr: desc.addr(),
                inner: desc,
            }));
        }

        let desc_addr = desc.addr();
        let desc_len = desc.len() as usize;
        let translated = match self.access_platform {
            Some(access_platform) => access_platform
                .translate_gva(desc_addr.0, desc_len as u64)
                .map(GuestAddress),
            None => Ok(desc_addr),
        };
        let result = translated.map_err(|_| desc_addr).and_then(|addr| {
            if self.chain.memory().check_range(addr, desc_len) {
                Ok(CheckedDescriptor { inner: desc, addr })
            } else {
                Err(addr)
            }
        });

        if let Err(addr) = result {
            warn!(
                "Rejecting virtio descriptor buffer at 0x{:x} with length {}",
                addr.0, desc_len
            );
            self.done = true;
        }
        Some(result)
    }
}

/// Extension trait providing checked descriptor iteration.
pub trait DescriptorChainExt<M> {
    fn checked_iter<'a>(
        &'a mut self,
        access_platform: Option<&'a dyn AccessPlatform>,
    ) -> CheckedDescriptorIter<'a, M>;

    fn next_checked(
        &mut self,
        access_platform: Option<&dyn AccessPlatform>,
    ) -> Result<Option<CheckedDescriptor>, GuestAddress>;
}

impl<M> DescriptorChainExt<M> for DescriptorChain<M>
where
    M: Deref,
    M::Target: GuestMemory,
{
    fn checked_iter<'a>(
        &'a mut self,
        access_platform: Option<&'a dyn AccessPlatform>,
    ) -> CheckedDescriptorIter<'a, M> {
        CheckedDescriptorIter::new(self, access_platform)
    }

    fn next_checked(
        &mut self,
        access_platform: Option<&dyn AccessPlatform>,
    ) -> Result<Option<CheckedDescriptor>, GuestAddress> {
        match self.checked_iter(access_platform).next() {
            Some(Ok(desc)) => Ok(Some(desc)),
            Some(Err(addr)) => Err(addr),
            None => Ok(None),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::queue::testing::VirtQueue;
    use virtio_queue::{Queue, QueueT};
    use vm_memory::bitmap::AtomicBitmap;
    use vm_memory::{GuestAddress, GuestAddressSpace, GuestMemoryAtomic, GuestMemoryMmap};

    type TestMemory = GuestMemoryMmap<AtomicBitmap>;

    fn queue_for(
        mem_size: usize,
        addr: u64,
        len: u32,
    ) -> (TestMemory, GuestMemoryAtomic<TestMemory>, Queue) {
        let memory = TestMemory::from_ranges(&[(GuestAddress(0), mem_size)]).unwrap();
        let queue_memory = memory.clone();
        let guest_queue = VirtQueue::new(GuestAddress(0x1_0000), &queue_memory, 2);
        guest_queue.dtable[0].set(addr, len, 0, 0);
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.idx.set(1);
        let atomic = GuestMemoryAtomic::new(memory.clone());
        (memory, atomic, guest_queue.create_queue())
    }

    #[test]
    fn rejects_descriptor_that_crosses_guest_memory_end() {
        let (_memory, atomic, mut queue) = queue_for(0x20_000, 0x1_ff00, 0x200);
        let mut chain = queue.pop_descriptor_chain(atomic.memory()).unwrap();
        assert!(chain.next_checked(None).is_err());
    }

    #[test]
    fn accepts_descriptor_at_guest_memory_end() {
        let (_memory, atomic, mut queue) = queue_for(0x20_000, 0x1_ff00, 0x100);
        let mut chain = queue.pop_descriptor_chain(atomic.memory()).unwrap();
        let descriptor = chain.next_checked(None).unwrap().unwrap();
        assert_eq!(descriptor.addr(), GuestAddress(0x1_ff00));
        assert_eq!(descriptor.len(), 0x100);
    }
}
