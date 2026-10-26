// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import "regexp"

var (
	bdfPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]{1}$`)
)

func ValidateBDF(bdf string) bool {
	return bdfPattern.MatchString(bdf)
}
