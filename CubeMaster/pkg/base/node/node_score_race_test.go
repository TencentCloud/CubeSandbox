// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeScoreConcurrentAccess exercises the SetScore/GetScore atomic pair
// against concurrent readers (Clone, GetScore, JSON) so `go test -race`
// flags any non-atomic access to the score bits.
func TestNodeScoreConcurrentAccess(t *testing.T) {
	n := &Node{InsID: "race-node"}
	n.SetScore(1.5)

	const writers = 4
	const readers = 4
	const iterations = 2000

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(base float64) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				n.SetScore(base + float64(i%64))
			}
		}(float64(w) * 1000)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = n.GetScore()
				_ = n.Clone()
				data, err := json.Marshal(n)
				if err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
				var decoded Node
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Errorf("unmarshal: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	// The score remains a finite float64 after the storm.
	score := n.GetScore()
	assert.False(t, score != score, "score became NaN")
}

// TestNodeScoreJSONRoundTrip keeps the wire shape of "Score" stable while the
// field lives in atomic bits.
func TestNodeScoreJSONRoundTrip(t *testing.T) {
	n := &Node{InsID: "json-node"}
	n.SetScore(87.5)

	data, err := json.Marshal(n)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"Score":87.5`)

	var decoded Node
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, 87.5, decoded.GetScore())
	assert.Equal(t, 87.5, decoded.Clone().GetScore())

	// omitempty: a zero score must not appear on the wire.
	n.SetScore(0)
	data, err = json.Marshal(n)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Score")
}

// TestNodeUnmarshalMissingScoreKeepsValue pins the absent-key semantics: a
// JSON document without "Score" must not clear the current score, while an
// explicit 0 must.
func TestNodeUnmarshalMissingScoreKeepsValue(t *testing.T) {
	n := &Node{InsID: "n1"}
	n.SetScore(88.5)

	require.NoError(t, json.Unmarshal([]byte(`{"InstanceID":"n1"}`), n))
	assert.Equal(t, 88.5, n.GetScore(), "absent Score key must keep the current score")

	require.NoError(t, json.Unmarshal([]byte(`{"InstanceID":"n1","Score":0}`), n))
	assert.Equal(t, 0.0, n.GetScore(), "explicit zero must clear the score")
}
