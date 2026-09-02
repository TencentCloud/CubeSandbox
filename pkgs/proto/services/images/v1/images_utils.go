// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"encoding/json"

	"google.golang.org/protobuf/proto"
)

const (
	MasterAnnotationsImageUserName = "cube.master.image.username"
	MasterAnnotationsImagetoken    = "cube.master.image.token"
)

func (x *ImageSpec) GetUsername() string {
	return x.GetAnnotations()[MasterAnnotationsImageUserName]
}

func (x *ImageSpec) GetToken() string {
	return x.GetAnnotations()[MasterAnnotationsImagetoken]
}

func SafePrintImageSpec(imageReq *ImageSpec) string {
	if imageReq == nil {
		return "nil"
	}
	// Annotations carry registry credentials (MasterAnnotationsImagetoken /
	// MasterAnnotationsImageUserName); mask them before the spec is logged.
	// Clone (deep copy) instead of copying the struct by value -- a proto
	// message embeds protoimpl.MessageState (a sync.Mutex), so a value copy
	// trips go vet's copylocks and is unsafe to modify.
	safe := proto.Clone(imageReq).(*ImageSpec)
	if safe.Annotations != nil {
		ann := make(map[string]string, len(safe.Annotations))
		for k, v := range safe.Annotations {
			if k == MasterAnnotationsImagetoken || k == MasterAnnotationsImageUserName {
				v = "***"
			}
			ann[k] = v
		}
		safe.Annotations = ann
	}
	tmpdata, _ := json.Marshal(safe)
	return string(tmpdata)
}
