// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multimeta

import "os"

type mockMetadataAPI struct {
	data map[string][]byte
}

func (m *mockMetadataAPI) Get(bucket, key string) ([]byte, error) {
	fullKey := bucket + "/" + key
	if data, ok := m.data[fullKey]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}

func (m *mockMetadataAPI) SetWithTx(bucket, key string, value []byte, callback func() error) error {
	fullKey := bucket + "/" + key
	m.data[fullKey] = value
	if callback != nil {
		return callback()
	}
	return nil
}

func (m *mockMetadataAPI) DeleteWithTx(bucket, key string, callback func() error) error {
	fullKey := bucket + "/" + key
	delete(m.data, fullKey)
	if callback != nil {
		return callback()
	}
	return nil
}

func (m *mockMetadataAPI) ReadAll(bucket string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	prefix := bucket + "/"
	for k, v := range m.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			key := k[len(prefix):]
			result[key] = v
		}
	}
	return result, nil
}

func (m *mockMetadataAPI) GetBs(key string, buckets ...[]byte) ([]byte, error) {
	if len(buckets) > 0 {
		fullKey := string(buckets[0]) + "/" + key
		if data, ok := m.data[fullKey]; ok {
			return data, nil
		}
	}
	return nil, os.ErrNotExist
}

func (m *mockMetadataAPI) ReadAllBs(buckets ...[]byte) (map[string][]byte, error) {
	result := make(map[string][]byte)
	if len(buckets) > 0 {
		prefix := string(buckets[0]) + "/"
		for k, v := range m.data {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				key := k[len(prefix):]
				result[key] = v
			}
		}
	}
	return result, nil
}

func (m *mockMetadataAPI) Close() error {
	return nil
}

func NewMockMetadataAPI() *mockMetadataAPI {
	return &mockMetadataAPI{
		data: make(map[string][]byte),
	}
}

var _ MetadataDBAPI = &mockMetadataAPI{}
