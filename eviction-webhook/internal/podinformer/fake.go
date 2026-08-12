// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package podinformer

import corev1 "k8s.io/api/core/v1"

// Fake is a static, in-memory PodGetter test double keyed by "namespace/name".
// It is shared across the admission, integration, and e2e test suites so
// they don't each hand-roll the same lookup.
type Fake struct {
	Pods map[string]*corev1.Pod
}

// Get implements the PodGetter interface (namespace/name -> Pod lookup).
func (f *Fake) Get(namespace, name string) (*corev1.Pod, bool) {
	pod, ok := f.Pods[namespace+"/"+name]
	return pod, ok
}
