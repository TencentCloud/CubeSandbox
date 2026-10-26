// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func GetNICQueues(cpu int64) int64 {
	if cpu > 36 {
		return 4
	} else if cpu > 12 {
		return 2
	} else {
		return 1
	}
}

func RemoveDiskQuota(ctx context.Context, client oci.Client, ctr *containers.Container, spec *oci.Spec) error {
	diskOpt, ok := spec.Annotations[constants.AnnotationsMountListKey]
	if !ok {
		return nil
	}

	var diskConfig []CubeDiskConfig
	if err := json.Unmarshal([]byte(diskOpt), &diskConfig); err != nil {
		return fmt.Errorf("failed to unmarshal disk config: %v", err)
	}

	for i := range diskConfig {
		diskConfig[i].FSQuota = 0
	}

	diskAnno, err := json.Marshal(diskConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal disk config: %v", err)
	}

	spec.Annotations[constants.AnnotationsMountListKey] = string(diskAnno)

	return nil
}

func getPCIIDByDiskID(diskId string) (string, error) {
	devName, err := os.Readlink(fmt.Sprintf("/dev/disk/by-id/virtio-%s", diskId))
	if err != nil {
		return "", err
	}

	dirPath := "/dev/disk/by-path"
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}
	const pciPrefix = "pci-"
	var matchingFile string
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), pciPrefix) {
			continue
		}
		linkPath := fmt.Sprintf("%s/%s", dirPath, file.Name())
		link, err := os.Readlink(linkPath)
		if err != nil {
			continue
		}
		if link == devName {
			matchingFile = file.Name()
			break
		}
	}

	if matchingFile == "" {
		return "", fmt.Errorf("not found")
	}
	return strings.TrimPrefix(matchingFile, pciPrefix), nil
}

func GetPCIIDByDiskUUID(diskUuid string) (string, error) {
	if err := pathutil.ValidateUUID(diskUuid); err != nil {
		return "", fmt.Errorf("invalid disk uuid: %w", err)
	}
	if stdout, stderr, err := utils.ExecBin(config.GetCommon().GetBDFByUuidCmd,
		[]string{diskUuid}, config.GetCommon().CommandTimeout); err != nil {
		return "", fmt.Errorf("%v, %v, %v", err, string(stdout), string(stderr))
	} else {
		return strings.TrimSpace(stdout), nil
	}
}

func GetDeviceNameByBDF(bdf string) (string, error) {
	devName, err := os.Readlink(fmt.Sprintf("/dev/disk/by-path/pci-%s", bdf))
	if err != nil {
		return "", fmt.Errorf("fail to get device name by bdf: %s, err: %v", bdf, err)
	}
	return devName, nil
}
