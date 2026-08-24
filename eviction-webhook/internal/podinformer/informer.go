// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package podinformer maintains a local in-memory cache of Pod objects backed
// by a Kubernetes list-watch informer. It is used by the admission handler to
// look up Pod labels (in particular cube.master.instance.type) without making
// a live API call on the hot webhook path.
package podinformer

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Cache wraps a Pod lister backed by a shared informer.
type Cache struct {
	lister listersv1.PodLister
}

// New starts a Pod informer for the given namespace (empty string = all
// namespaces), waits for the cache to sync, then returns the ready Cache.
// The informer runs until ctx is cancelled.
func New(ctx context.Context, client kubernetes.Interface, namespace string) (*Cache, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		30*time.Second,
		informers.WithNamespace(namespace),
	)

	podInformer := factory.Core().V1().Pods()
	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
		return nil, fmt.Errorf("timed out waiting for Pod cache to sync")
	}

	return &Cache{lister: podInformer.Lister()}, nil
}

// NewWithSyncTimeout starts a Pod informer, waits up to syncTimeout for the
// cache to sync, then returns the ready Cache. The informer runs until ctx is
// cancelled, but the sync wait uses a separate context with the given timeout.
func NewWithSyncTimeout(ctx context.Context, client kubernetes.Interface, namespace string, syncTimeout time.Duration) (*Cache, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		30*time.Second,
		informers.WithNamespace(namespace),
	)

	podInformer := factory.Core().V1().Pods()
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(context.Background(), syncTimeout)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), podInformer.Informer().HasSynced) {
		return nil, fmt.Errorf("timed out waiting for Pod cache to sync after %s", syncTimeout)
	}
	log.Printf("[podinformer] cache synced (namespace=%q)", namespace)

	return &Cache{lister: podInformer.Lister()}, nil
}

// NewAsync starts a Pod informer without blocking startup. It waits up to
// 10 seconds for the cache to sync in the background so that most pods are
// cached by the time the first eviction request arrives. If sync takes
// longer, the webhook starts serving anyway and the cache fills in
// asynchronously.
func NewAsync(ctx context.Context, client kubernetes.Interface, namespace string) (*Cache, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		30*time.Second,
		informers.WithNamespace(namespace),
	)

	podInformer := factory.Core().V1().Pods()
	factory.Start(ctx.Done())

	// Wait briefly for cache sync so pods are available for the first request.
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer syncCancel()
	if cache.WaitForCacheSync(syncCtx.Done(), podInformer.Informer().HasSynced) {
		log.Printf("[podinformer] cache synced (namespace=%q)", namespace)
	} else {
		log.Printf("[podinformer] cache sync not yet complete (namespace=%q), continuing in background", namespace)
		go func() {
			if cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				log.Printf("[podinformer] cache synced (namespace=%q)", namespace)
			} else {
				log.Printf("[podinformer] cache sync cancelled (namespace=%q)", namespace)
			}
		}()
	}

	return &Cache{lister: podInformer.Lister()}, nil
}

// Get returns the Pod from the local cache. ok is false when the Pod is not
// cached (rare: Pod was deleted before the webhook fired, or cache is lagging).
func (c *Cache) Get(namespace, name string) (*corev1.Pod, bool) {
	pod, err := c.lister.Pods(namespace).Get(name)
	if err != nil {
		return nil, false
	}
	return pod, true
}
