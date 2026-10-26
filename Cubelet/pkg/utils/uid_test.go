// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateID(t *testing.T) {
	t.Run("valid hex format and length", func(t *testing.T) {
		got := GenerateID()
		assert.Len(t, got, 32, "ID length should be 32 characters")
		_, err := hex.DecodeString(got)
		assert.Nil(t, err, "ID should be valid hex string")
	})

	t.Run("uniqueness between multiple calls", func(t *testing.T) {
		id1 := GenerateID()
		id2 := GenerateID()
		assert.NotEqual(t, id1, id2, "Generated IDs should be unique")
	})

	t.Run("zero value handling", func(t *testing.T) {
		got := GenerateID()
		assert.NotEmpty(t, got, "Generated ID should not be empty")
	})
}
