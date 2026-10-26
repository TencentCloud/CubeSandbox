// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/runtime/protoiface"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1"
	"sigs.k8s.io/yaml"
)

const (
	outputTypeJSON       = "json"
	outputTypeYAML       = "yaml"
	outputTypeTable      = "table"
	outputTypeGoTemplate = "go-template"
)

func protobufObjectToJSON(obj protoiface.MessageV1) (string, error) {
	msg := protoadapt.MessageV2Of(obj)

	marshaledJSON, err := protojson.MarshalOptions{EmitDefaultValues: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		return "", err
	}

	return string(marshaledJSON), nil
}

func outputProtobufObjAsJSON(obj protoiface.MessageV1) error {
	marshaledJSON, err := protobufObjectToJSON(obj)
	if err != nil {
		return err
	}

	fmt.Println(marshaledJSON)

	return nil
}

func outputProtobufObjAsYAML(obj protoiface.MessageV1) error {
	marshaledJSON, err := protobufObjectToJSON(obj)
	if err != nil {
		return err
	}

	marshaledYAML, err := yaml.JSONToYAML([]byte(marshaledJSON))
	if err != nil {
		return err
	}

	fmt.Println(string(marshaledYAML))

	return nil
}

func printJSONSchema(value any) error {
	schema := jsonschema.Reflect(value)

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON schema: %w", err)
	}

	fmt.Println(string(data))

	return nil
}

func loadPodSandboxConfig(path string) (*pb.PodSandboxConfig, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var config pb.PodSandboxConfig
	if err := utilyaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&config); err != nil {
		return nil, err
	}

	if config.Metadata == nil {
		return nil, errors.New("metadata is not set")
	}

	if config.Metadata.Name == "" || config.Metadata.Namespace == "" || config.Metadata.Uid == "" {
		return nil, fmt.Errorf("name, namespace or uid is not in metadata %q", config.Metadata)
	}

	return &config, nil
}

func openFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config at %s not found", path)
		}

		return nil, err
	}

	return f, nil
}

func parseLabelStringSlice(ss []string) (map[string]string, error) {
	labels := make(map[string]string)

	for _, s := range ss {
		pair := strings.Split(s, "=")
		if len(pair) != 2 {
			return nil, fmt.Errorf("incorrectly specified label: %v", s)
		}

		labels[pair[0]] = pair[1]
	}

	return labels, nil
}

type statusData struct {
	json            string
	runtimeHandlers string
	features        string
	info            map[string]string
}

func outputStatusData(statuses []statusData, format, tmplStr string) (err error) {
	if len(statuses) == 0 {
		return nil
	}

	result := []map[string]any{}

	for _, status := range statuses {

		keys := []string{}
		for k := range status.info {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		infoMap := map[string]any{}

		if status.json != "" {
			var statusVal map[string]any

			err := json.Unmarshal([]byte(status.json), &statusVal)
			if err != nil {
				return fmt.Errorf("unmarshal status JSON: %w", err)
			}

			infoMap["status"] = statusVal
		}

		if status.runtimeHandlers != "" {
			var handlersVal []*any

			err := json.Unmarshal([]byte(status.runtimeHandlers), &handlersVal)
			if err != nil {
				return fmt.Errorf("unmarshal runtime handlers: %w", err)
			}

			if handlersVal != nil {
				infoMap["runtimeHandlers"] = handlersVal
			}
		}

		if status.features != "" {
			var featuresVal map[string]any = map[string]any{}

			err := json.Unmarshal([]byte(status.features), &featuresVal)
			if err != nil {
				return fmt.Errorf("unmarshal features JSON: %w", err)
			}

			if featuresVal != nil {
				infoMap["features"] = featuresVal
			}
		}

		for _, k := range keys {
			val := status.info[k]

			if strings.HasPrefix(val, "{") {

				var genericVal map[string]any
				if err := json.Unmarshal([]byte(val), &genericVal); err != nil {
					return fmt.Errorf("unmarshal status info JSON: %w", err)
				}

				infoMap[k] = genericVal
			} else {

				infoMap[k] = strings.Trim(val, `"`)
			}
		}

		result = append(result, infoMap)
	}

	var jsonResult []byte
	if len(result) == 1 {
		jsonResult, err = json.Marshal(result[0])
	} else {
		jsonResult, err = json.Marshal(result)
	}

	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	switch format {
	case outputTypeYAML:
		yamlInfo, err := yaml.JSONToYAML(jsonResult)
		if err != nil {
			return fmt.Errorf("JSON result to YAML: %w", err)
		}

		fmt.Println(string(yamlInfo))
	case outputTypeJSON:
		var output bytes.Buffer
		if err := json.Indent(&output, jsonResult, "", "  "); err != nil {
			return fmt.Errorf("indent JSON result: %w", err)
		}

		fmt.Println(output.String())
	case outputTypeGoTemplate:
		output, err := tmplExecuteRawJSON(tmplStr, string(jsonResult))
		if err != nil {
			return fmt.Errorf("execute template: %w", err)
		}

		fmt.Println(output)
	default:
		return fmt.Errorf("unsupported format: %q", format)
	}

	return nil
}
