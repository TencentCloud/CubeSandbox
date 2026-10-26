// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use std::collections::hash_map::IterMut;
use std::collections::HashMap;
use std::io;
use std::ops::{Index, IndexMut};
use std::slice::SliceIndex;

/// Trait that allows for checking if an implementor is dirty. Useful for types that are cached so
/// it can be checked if they need to be committed to disk.
pub trait Cacheable {
    /// Used to check if the item needs to be written out or if it can be discarded.
    fn dirty(&self) -> bool;
}

#[derive(Clone, Debug)]
/// Represents a vector that implements the `Cacheable` trait so it can be held in a cache.
pub struct VecCache<T: 'static + Copy + Default> {
    vec: Box<[T]>,
    dirty: bool,
}

impl<T: 'static + Copy + Default> VecCache<T> {
    /// Creates a `VecCache` that can hold `count` elements.
    pub fn new(count: usize) -> VecCache<T> {
        VecCache {
            vec: vec![Default::default(); count].into_boxed_slice(),
            dirty: true,
        }
    }

    /// Creates a `VecCache` from the passed in `vec`.
    pub fn from_vec(vec: Vec<T>) -> VecCache<T> {
        VecCache {
            vec: vec.into_boxed_slice(),
            dirty: false,
        }
    }

    pub fn get<I>(&self, index: I) -> Option<&<I as SliceIndex<[T]>>::Output>
    where
        I: SliceIndex<[T]>,
    {
        self.vec.get(index)
    }

    /// Gets a reference to the underlying vector.
    pub fn get_values(&self) -> &[T] {
        &self.vec
    }

    /// Mark this cache element as clean.
    pub fn mark_clean(&mut self) {
        self.dirty = false;
    }

    /// Returns the number of elements in the vector.
    pub fn len(&self) -> usize {
        self.vec.len()
    }
}

impl<T: 'static + Copy + Default> Cacheable for VecCache<T> {
    fn dirty(&self) -> bool {
        self.dirty
    }
}

impl<T: 'static + Copy + Default> Index<usize> for VecCache<T> {
    type Output = T;

    fn index(&self, index: usize) -> &T {
        self.vec.index(index)
    }
}

impl<T: 'static + Copy + Default> IndexMut<usize> for VecCache<T> {
    fn index_mut(&mut self, index: usize) -> &mut T {
        self.dirty = true;
        self.vec.index_mut(index)
    }
}

#[derive(Clone, Debug)]
pub struct CacheMap<T: Cacheable> {
    capacity: usize,
    map: HashMap<usize, T>,
}

impl<T: Cacheable> CacheMap<T> {
    pub fn new(capacity: usize) -> Self {
        CacheMap {
            capacity,
            map: HashMap::with_capacity(capacity),
        }
    }

    pub fn contains_key(&self, key: usize) -> bool {
        self.map.contains_key(&key)
    }

    pub fn get(&self, index: usize) -> Option<&T> {
        self.map.get(&index)
    }

    pub fn get_mut(&mut self, index: usize) -> Option<&mut T> {
        self.map.get_mut(&index)
    }

    pub fn iter_mut(&mut self) -> IterMut<usize, T> {
        self.map.iter_mut()
    }

    // Check if the refblock cache is full and we need to evict.
    pub fn insert<F>(&mut self, index: usize, block: T, write_callback: F) -> io::Result<()>
    where
        F: FnOnce(usize, T) -> io::Result<()>,
    {
        if self.map.len() == self.capacity {
            // TODO(dgreid) - smarter eviction strategy.
            let to_evict = *self.map.iter().next().unwrap().0;
            if let Some(evicted) = self.map.remove(&to_evict) {
                if evicted.dirty() {
                    write_callback(to_evict, evicted)?;
                }
            }
        }
        self.map.insert(index, block);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct NumCache(());
    impl Cacheable for NumCache {
        fn dirty(&self) -> bool {
            true
        }
    }

    #[test]
    fn evicts_when_full() {
        let mut cache = CacheMap::<NumCache>::new(3);
        let mut evicted = None;
        cache
            .insert(0, NumCache(()), |index, _| {
                evicted = Some(index);
                Ok(())
            })
            .unwrap();
        assert_eq!(evicted, None);
        cache
            .insert(1, NumCache(()), |index, _| {
                evicted = Some(index);
                Ok(())
            })
            .unwrap();
        assert_eq!(evicted, None);
        cache
            .insert(2, NumCache(()), |index, _| {
                evicted = Some(index);
                Ok(())
            })
            .unwrap();
        assert_eq!(evicted, None);
        cache
            .insert(3, NumCache(()), |index, _| {
                evicted = Some(index);
                Ok(())
            })
            .unwrap();
        assert!(evicted.is_some());

        // Check that three of the four items inserted are still there and that the most recently
        // inserted is one of them.
        let num_items = (0..=3).filter(|k| cache.contains_key(*k)).count();
        assert_eq!(num_items, 3);
        assert!(cache.contains_key(3));
    }
}
