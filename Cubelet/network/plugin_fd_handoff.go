// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"fmt"
	"os"
)

// GetTapFileForShim obtains a one-shot TAP file from the runtime. The caller
// owns the returned file and must close it after passing it via SCM_RIGHTS.
func GetTapFileForShim(sandboxID, requestedTapName string) (*os.File, error) {
	if dnm == nil || dnm.tapPlugin == nil || dnm.tapPlugin.networkRuntime == nil {
		return nil, fmt.Errorf("network runtime is not initialized")
	}
	return dnm.tapPlugin.getTapFileForShim(sandboxID, requestedTapName)
}

func (l *local) getTapFileForShim(sandboxID, requestedTapName string) (*os.File, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox id is empty")
	}
	if requestedTapName == "" {
		return nil, fmt.Errorf("tap name is empty for sandbox %q", sandboxID)
	}

	file, err := l.networkRuntime.GetTapFile(sandboxID, requestedTapName)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("network runtime returned a nil tap file for sandbox %q tap %q", sandboxID, requestedTapName)
	}
	return file, nil
}
