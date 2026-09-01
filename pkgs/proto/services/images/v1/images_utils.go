// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import "encoding/json"

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
	safe := *imageReq
	if imageReq.Annotations != nil {
		ann := make(map[string]string, len(imageReq.Annotations))
		for k, v := range imageReq.Annotations {
			if k == MasterAnnotationsImagetoken || k == MasterAnnotationsImageUserName {
				v = "***"
			}
			ann[k] = v
		}
		safe.Annotations = ann
	}
	tmpdata, _ := json.Marshal(&safe)
	return string(tmpdata)
}
