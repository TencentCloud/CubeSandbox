// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

func parseCreateTemplateFromImageRequest(r *http.Request) (*types.CreateTemplateFromImageReq, *templatecenter.EnvdInjectionPayload, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "application/json"
	}
	switch mediaType {
	case "application/json":
		req := &types.CreateTemplateFromImageReq{}
		if err := common.GetBodyReq(r, req); err != nil {
			return nil, nil, err
		}
		if templatecenter.ShouldInjectEnvdIntoTemplate(req) {
			return nil, nil, fmt.Errorf("envd injection requires multipart create-from-image upload")
		}
		return req, nil, nil
	case "multipart/form-data":
		return parseMultipartCreateTemplateFromImageRequest(r)
	default:
		return nil, nil, fmt.Errorf("unsupported content type %q", mediaType)
	}
}

func parseMultipartCreateTemplateFromImageRequest(r *http.Request) (*types.CreateTemplateFromImageReq, *templatecenter.EnvdInjectionPayload, error) {
	if err := r.ParseMultipartForm(constants.MaxEnvdPayloadBytes + 1*1024*1024); err != nil {
		return nil, nil, err
	}
	values := r.MultipartForm.Value["request"]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return nil, nil, fmt.Errorf("multipart field %q is required", "request")
	}
	req := &types.CreateTemplateFromImageReq{}
	if err := jsoniter.Unmarshal([]byte(values[0]), req); err != nil {
		return nil, nil, fmt.Errorf("decode multipart request field: %w", err)
	}
	if !templatecenter.ShouldInjectEnvdIntoTemplate(req) {
		return req, nil, nil
	}
	files := r.MultipartForm.File["envd"]
	if len(files) != 1 {
		return nil, nil, fmt.Errorf("multipart file field %q is required when envd injection is enabled", "envd")
	}
	file, err := files[0].Open()
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, constants.MaxEnvdPayloadBytes+1))
	if err != nil {
		return nil, nil, err
	}
	payload, err := templatecenter.NewEnvdInjectionPayloadFromBytes(data)
	if err != nil {
		return nil, nil, err
	}
	return req, payload, nil
}
