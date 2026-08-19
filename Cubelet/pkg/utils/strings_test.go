// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_HashCode(t *testing.T) {
	size := 10000
	cases := make([]string, 0, size)
	for i := 0; i < size; i++ {
		cases = append(cases, strconv.Itoa(int(rand.Int31n(int32(size)))))
	}

	m := make(map[string]uint32, size)
	for _, c := range cases {
		m[c] = HashCode(c)
	}
	for _, c := range cases {
		if code := HashCode(c); code != m[c] {
			t.Fatalf("hashCode not stable for %q: got %d, first saw %d", c, code, m[c])
		}
	}
	if len(m) == 0 {
		t.Fatal("no cases were generated")
	}
}

func Benchmark_HashCode(b *testing.B) {
	inputs := make([]string, 0, 1024)
	for i := 0; i < 1024; i++ {
		inputs = append(inputs, strconv.Itoa(int(rand.Int31n(1024))))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HashCode(inputs[i%len(inputs)])
	}
}

func TestString2Slice(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected []byte
	}{
		{
			name:     "empty string",
			input:    "",
			expected: []byte{},
		},
		{
			name:     "non-empty string",
			input:    "hello world",
			expected: []byte{104, 101, 108, 108, 111, 32, 119, 111, 114, 108, 100},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := String2Slice(tc.input)
			if !bytes.Equal(actual, tc.expected) {
				t.Errorf("expected %v, but got %v", tc.expected, actual)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	type testStruct struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	testCases := []struct {
		name     string
		input    string
		expected testStruct
	}{
		{
			name:     "empty string",
			input:    "{}",
			expected: testStruct{},
		},
		{
			name:     "non-empty string",
			input:    `{"name":"John","age":30}`,
			expected: testStruct{Name: "John", Age: 30},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := testStruct{}
			err := Decode(tc.input, &actual)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			assert.Equal(t, tc.expected, actual)
		})
	}
}

func TestInterfaceToString(t *testing.T) {
	type testStruct struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	testCases := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "empty struct",
			input:    testStruct{},
			expected: `{"name":"","age":0}`,
		},
		{
			name:     "non-empty struct",
			input:    testStruct{Name: "John", Age: 30},
			expected: `{"name":"John","age":30}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := InterfaceToString(tc.input)
			if actual != tc.expected {
				t.Errorf("expected %v, but got %v", tc.expected, actual)
			}
		})
	}
}

func TestReadAtMost(t *testing.T) {
	testCases := []struct {
		name         string
		input        string
		limit        int64
		expectedData []byte
		expectedErr  error
	}{
		{
			name:         "empty string",
			input:        "",
			limit:        10,
			expectedData: []byte{},
			expectedErr:  nil,
		},
		{
			name:         "non-empty string",
			input:        "hello world",
			limit:        5,
			expectedData: []byte("hello"),
			expectedErr:  ErrLimitReached,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualData, actualErr := ReadAtMost(strings.NewReader(tc.input), tc.limit)
			assert.Equal(t, tc.expectedData, actualData)
			if !errors.Is(actualErr, tc.expectedErr) {
				t.Errorf("expected %v, but got %v", tc.expectedErr, actualErr)
			}
		})
	}
}

func TestMaxCommonPrefix(t *testing.T) {
	testCases := []struct {
		name     string
		input    []string
		expected string
	}{
		{
			name:     "empty array",
			input:    []string{},
			expected: "",
		},
		{
			name:     "single string",
			input:    []string{"hello"},
			expected: "hello",
		},
		{
			name:     "multiple strings with common prefix",
			input:    []string{"flower", "flow", "flight"},
			expected: "fl",
		},
		{
			name:     "no common prefix",
			input:    []string{"dog", "racecar", "car"},
			expected: "",
		},
		{
			name:     "all strings identical",
			input:    []string{"test", "test", "test"},
			expected: "test",
		},
		{
			name:     "one string is prefix of others",
			input:    []string{"inter", "interspecies", "interstellar", "interstate"},
			expected: "inter",
		},
		{
			name:     "contains empty string",
			input:    []string{"hello", "", "world"},
			expected: "",
		},
		{
			name:     "all empty strings",
			input:    []string{"", "", ""},
			expected: "",
		},
		{
			name:     "long common prefix",
			input:    []string{"interspecies", "interstellar", "interstate"},
			expected: "inters",
		},
		{
			name:     "single character common prefix",
			input:    []string{"abc", "ade", "afg"},
			expected: "a",
		},
		{
			name:     "case sensitive",
			input:    []string{"Hello", "hello", "HELLO"},
			expected: "",
		},
		{
			name:     "unicode characters",
			input:    []string{"你好世界", "你好", "你好吗"},
			expected: "你好",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := MaxCommonPrefix(tc.input)
			assert.Equal(t, tc.expected, actual, "MaxCommonPrefix(%v) = %q, want %q", tc.input, actual, tc.expected)
		})
	}
}
