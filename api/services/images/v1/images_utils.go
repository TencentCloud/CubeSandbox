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
	tmpdata, _ := json.Marshal(imageReq)
	return string(tmpdata)
}
