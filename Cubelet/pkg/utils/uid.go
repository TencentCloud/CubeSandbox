// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"encoding/hex"

	"github.com/google/uuid"
)

func GenerateID() string {
	uuidObj := uuid.New()
	return hex.EncodeToString(uuidObj[:])
}
