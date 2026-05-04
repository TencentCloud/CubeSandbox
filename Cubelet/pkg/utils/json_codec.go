// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"encoding/json"
)

type JSONCodec struct {
	useNumber bool
}

func NewJSONCodec(useNumber bool) JSONCodec {
	return JSONCodec{useNumber: useNumber}
}

func (c JSONCodec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (c JSONCodec) Unmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if c.useNumber {
		dec.UseNumber()
	}
	return dec.Decode(v)
}

func (c JSONCodec) UnmarshalFromString(data string, v any) error {
	return c.Unmarshal([]byte(data), v)
}
