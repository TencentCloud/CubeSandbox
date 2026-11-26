// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplatePreStopAttemptSurvivesStatusPersistence(t *testing.T) {
	status := StoreStatus(Status{TemplatePreStopStarted: true, ExitUnobserved: true})
	data, err := json.Marshal(status)
	require.NoError(t, err)
	var restored StatusStorage
	require.NoError(t, json.Unmarshal(data, &restored))
	require.True(t, restored.Get().TemplatePreStopStarted,
		"recovery must not repeat a preStop hook that was already attempted")
	require.False(t, restored.Get().ExitUnobserved,
		"the recovery-only exit marker must remain transient")
}
