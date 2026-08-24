// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package nodewatch monitors Kubernetes Node objects for MemoryPressure
// condition changes. When a node's MemoryPressure transitions from True to
// False the registered callback is invoked with the node name so the recovery
// manager can uncordon the node and resume its paused sandboxes.
package nodewatch

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PressureReliefFunc is called when a node's MemoryPressure condition clears.
type PressureReliefFunc func(nodeName string)

// PressureDetectedFunc is called when a node's MemoryPressure condition first
// transitions to True. This allows the recovery manager to proactively cordon
// the node and pause sandboxes without waiting for an API Eviction request,
// since kubelet's internal eviction bypasses the API server entirely.
type PressureDetectedFunc func(nodeName string)

// Watcher starts a Node informer and calls onRelief whenever a node's
// MemoryPressure condition transitions True → False. It runs until ctx is
// cancelled.
func Start(ctx context.Context, client kubernetes.Interface, onRelief PressureReliefFunc) error {
	return StartWithPressureDetected(ctx, client, onRelief, nil)
}

// StartWithPressureDetected is like Start but also calls onPressureDetected
// when MemoryPressure transitions False → True.
func StartWithPressureDetected(ctx context.Context, client kubernetes.Interface, onRelief PressureReliefFunc, onPressureDetected PressureDetectedFunc) error {
	factory := informers.NewSharedInformerFactory(client, 30*time.Second)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	// pressured tracks nodes currently under memory pressure so we can detect
	// the True → False edge.
	pressured := make(map[string]bool)
	var mu sync.Mutex

	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				return
			}
			mu.Lock()
			pressured[node.Name] = HasMemoryPressure(node)
			mu.Unlock()
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newNode, ok2 := newObj.(*corev1.Node)
			if !ok2 {
				return
			}

			mu.Lock()
			wasUnderPressure := pressured[newNode.Name]
			isUnderPressure := HasMemoryPressure(newNode)
			pressured[newNode.Name] = isUnderPressure
			mu.Unlock()

			if wasUnderPressure && !isUnderPressure {
				log.Printf("[nodewatch] MemoryPressure cleared node=%s", newNode.Name)
				onRelief(newNode.Name)
			}

			if !wasUnderPressure && isUnderPressure {
				log.Printf("[nodewatch] MemoryPressure detected node=%s", newNode.Name)
				if onPressureDetected != nil {
					onPressureDetected(newNode.Name)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					node, ok = tombstone.Obj.(*corev1.Node)
					if !ok {
						return
					}
				} else {
					return
				}
			}
			mu.Lock()
			delete(pressured, node.Name)
			mu.Unlock()
		},
	}); err != nil {
		return fmt.Errorf("add node event handler: %w", err)
	}

	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for Node cache to sync")
	}

	log.Printf("[nodewatch] node cache synced, watching MemoryPressure transitions")
	return nil
}

// StartAsync starts a Node informer without waiting for cache sync.
// The informer runs in the background; cache will sync eventually.
// Returns immediately so the webhook can start serving.
func StartAsync(ctx context.Context, client kubernetes.Interface, onRelief PressureReliefFunc) error {
	return StartAsyncWithPressureDetected(ctx, client, onRelief, nil)
}

// StartAsyncWithPressureDetected is like StartAsync but also calls
// onPressureDetected when MemoryPressure transitions False → True.
func StartAsyncWithPressureDetected(ctx context.Context, client kubernetes.Interface, onRelief PressureReliefFunc, onPressureDetected PressureDetectedFunc) error {
	factory := informers.NewSharedInformerFactory(client, 30*time.Second)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	pressured := make(map[string]bool)
	var mu sync.Mutex

	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				return
			}
			mu.Lock()
			pressured[node.Name] = HasMemoryPressure(node)
			mu.Unlock()
		},
		UpdateFunc: func(_ interface{}, newObj interface{}) {
			newNode, ok := newObj.(*corev1.Node)
			if !ok {
				return
			}

			mu.Lock()
			wasUnderPressure := pressured[newNode.Name]
			isUnderPressure := HasMemoryPressure(newNode)
			pressured[newNode.Name] = isUnderPressure
			mu.Unlock()

			if wasUnderPressure && !isUnderPressure {
				log.Printf("[nodewatch] MemoryPressure cleared node=%s", newNode.Name)
				onRelief(newNode.Name)
			}

			if !wasUnderPressure && isUnderPressure {
				log.Printf("[nodewatch] MemoryPressure detected node=%s", newNode.Name)
				if onPressureDetected != nil {
					onPressureDetected(newNode.Name)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					node, ok = tombstone.Obj.(*corev1.Node)
					if !ok {
						return
					}
				} else {
					return
				}
			}
			mu.Lock()
			delete(pressured, node.Name)
			mu.Unlock()
		},
	}); err != nil {
		return fmt.Errorf("add node event handler: %w", err)
	}

	factory.Start(ctx.Done())

	go func() {
		if cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced) {
			log.Printf("[nodewatch] node cache synced, watching MemoryPressure transitions")
		} else {
			log.Printf("[nodewatch] cache sync cancelled")
		}
	}()

	return nil
}

// HasMemoryPressure returns true when the node currently has the MemoryPressure
// condition set to True.
func HasMemoryPressure(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeMemoryPressure {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// HasResourcePressure returns true when the node currently reports any
// resource pressure condition that can drive kubelet evictions.
func HasResourcePressure(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		switch cond.Type {
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if cond.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}
