// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type imageRecord struct {
	created time.Time

	lastUsed time.Time

	size int64

	pinned bool
}

type evictionInfo struct {
	id string
	imageRecord
}

func (i evictionInfo) String() string {
	return fmt.Sprintf("id: %v, lastUsed: %s, created: %s, size %v",
		i.id, i.lastUsed.Format(time.RFC3339), i.created.Format(time.RFC3339), i.size)
}

func evictionByCreatedTime(e1, e2 *evictionInfo) bool {
	return e1.created.Before(e2.created)
}

func evictionByLastUsedTime(e1, e2 *evictionInfo) bool {
	return e1.lastUsed.Before(e2.lastUsed)
}

func evictionBySize(e1, e2 *evictionInfo) bool {
	return e1.size < e2.size
}

func TestMultiSort(t *testing.T) {
	now := time.Now()
	arr := []evictionInfo{
		{
			id: "1",
			imageRecord: imageRecord{
				created:  now.Add(-time.Hour),
				lastUsed: now.Add(-time.Hour),
				size:     1,
				pinned:   false,
			},
		},
		{
			id: "2",
			imageRecord: imageRecord{
				created:  now.Add(-time.Hour),
				lastUsed: now.Add(-time.Hour),
				size:     2,
				pinned:   false,
			},
		},
		{
			id: "3",
			imageRecord: imageRecord{
				created:  now.Add(-2 * time.Hour),
				lastUsed: now.Add(-time.Hour),
				size:     2,
				pinned:   false,
			},
		},
		{
			id: "4",
			imageRecord: imageRecord{
				created:  now.Add(2 * time.Hour),
				lastUsed: now.Add(-time.Hour),
				size:     2,
				pinned:   false,
			},
		},
		{
			id: "5",
			imageRecord: imageRecord{
				created:  now.Add(time.Hour),
				lastUsed: now.Add(-2 * time.Hour),
				size:     2,
				pinned:   false,
			},
		},
		{
			id: "6",
			imageRecord: imageRecord{
				created:  now.Add(time.Hour),
				lastUsed: now.Add(2 * time.Hour),
				size:     2,
				pinned:   false,
			},
		},
	}

	OrderedBy(evictionByLastUsedTime, evictionByCreatedTime, evictionBySize).Sort(arr)

	var sortedId string
	for _, v := range arr {
		sortedId += v.id
	}
	assert.Equal(t, "531246", sortedId)
}
