// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"os"
	"strings"

	jsoniter "github.com/json-iterator/go"
	embeddedenvd "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/internal/envd"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/urfave/cli"
)

type envdUploadPayload struct {
	Data   []byte
	Source string
}

func selectEnvdUploadPayload(c *cli.Context) (*envdUploadPayload, error) {
	envdPath := strings.TrimSpace(c.String("envd-path"))
	if envdPath != "" && !c.Bool("enable-inject-envd") {
		return nil, errors.New("--envd-path requires --enable-inject-envd")
	}
	if !c.Bool("enable-inject-envd") {
		return nil, nil
	}
	if envdPath != "" {
		data, err := readLocalEnvdBinary(envdPath)
		if err != nil {
			return nil, err
		}
		return &envdUploadPayload{Data: data, Source: envdPath}, nil
	}
	if embeddedenvd.HasDefaultBinary() {
		data := embeddedenvd.DefaultBinary()
		if err := validateEnvdUploadBytes("embedded envd", data); err != nil {
			return nil, err
		}
		return &envdUploadPayload{Data: data, Source: "embedded"}, nil
	}
	return nil, errors.New("envd injection is enabled, but no --envd-path was provided and this cubemastercli was built without an embedded envd binary")
}

func readLocalEnvdBinary(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read local envd binary %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("local envd binary %q must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read local envd binary %q: %w", path, err)
	}
	if err := validateEnvdUploadBytes(path, data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateEnvdUploadBytes(source string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("envd binary %q must not be empty", source)
	}
	if len(data) > constants.MaxEnvdPayloadBytes {
		return fmt.Errorf("envd binary %q exceeds 16MiB", source)
	}
	if len(data) < 4 || data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' {
		return fmt.Errorf("envd binary %q must be an ELF binary", source)
	}
	return nil
}

func buildCreateFromImageMultipartBody(req interface{}, payload *envdUploadPayload) (*bytes.Buffer, string, error) {
	if payload == nil {
		return nil, "", errors.New("envd upload payload is required")
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	requestPart, err := writer.CreateFormField("request")
	if err != nil {
		return nil, "", err
	}
	encoded, err := jsoniter.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	if _, err := requestPart.Write(encoded); err != nil {
		return nil, "", err
	}
	envdPart, err := writer.CreateFormFile("envd", "envd")
	if err != nil {
		return nil, "", err
	}
	if _, err := envdPart.Write(payload.Data); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body, writer.FormDataContentType(), nil
}
