// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import "time"

const (
	CubeMsgDevDefaultName = "/dev/cubemsg0"
)

var (
	DefaultCosProtocol                    = "https"
	DefaultUserCodeDownloadRetryNum       = 3
	DefaultUserCodeDownloadRetrySleepTime = time.Duration(10) * time.Millisecond
	DefaultCodeSliceSize                  = int64(5)
	DefaultCodeSliceMax                   = 8
)
