// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package util

import (
	"context"
	"path"
	"time"

	clabels "github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

const deferCleanupTimeout = 1 * time.Minute

func DeferContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(NamespacedContext(), deferCleanupTimeout)
}

func NamespacedContext() context.Context {
	return WithNamespace(context.Background())
}

func WithNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, constants.CubeDefaultNamespace)
}

func GetPassthroughAnnotations(podAnnotations map[string]string,
	runtimePodAnnotations []string) (passthroughAnnotations map[string]string) {
	passthroughAnnotations = make(map[string]string)

	for podAnnotationKey, podAnnotationValue := range podAnnotations {
		for _, pattern := range runtimePodAnnotations {

			if ok, _ := path.Match(pattern, podAnnotationKey); ok {
				passthroughAnnotations[podAnnotationKey] = podAnnotationValue
			}
		}
	}
	return passthroughAnnotations
}

func BuildLabels(configLabels, imageConfigLabels map[string]string) map[string]string {
	labels := make(map[string]string)

	for k, v := range imageConfigLabels {
		if err := clabels.Validate(k, v); err == nil {
			labels[k] = v
		} else {

			log.L.WithError(err).Warnf("unable to add image label with key %s to the container", k)
		}
	}

	for k, v := range configLabels {
		labels[k] = v
	}
	return labels
}
