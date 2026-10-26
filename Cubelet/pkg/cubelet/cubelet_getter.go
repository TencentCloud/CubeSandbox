// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"

	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
)

func (kl *Cubelet) GetNode() (*cubeletnodemeta.Node, error) {
	if kl.lastNodeSnapshot == nil {
		return kl.initialNode(context.TODO())
	}
	return kl.lastNodeSnapshot.DeepCopy(), nil
}
