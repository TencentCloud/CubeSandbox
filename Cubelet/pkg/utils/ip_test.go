// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInetNtoAv4(t *testing.T) {
	testCases := []struct {
		input    uint
		expected string
	}{
		{3232235521, "192.168.0.1"},
		{2886794753, "172.16.254.1"},
		{2147483648, "128.0.0.0"},
	}

	for _, tc := range testCases {
		actual := InetNtoAv4(tc.input)
		assert.Equal(t, tc.expected, actual, fmt.Sprintf("Input: %d", tc.input))
	}
}

func TestInetAtoNv4(t *testing.T) {
	testCases := []struct {
		input    string
		expected uint
		err      error
	}{
		{"192.168.0.1", 3232235521, nil},
		{"172.16.254.1", 2886794753, nil},
		{"128.0.0.0", 2147483648, nil},
		{"invalid", 0, fmt.Errorf("invalid ip")},
	}

	for _, tc := range testCases {
		actual, err := InetAtoNv4(tc.input)
		assert.Equal(t, tc.expected, actual, fmt.Sprintf("Input: %s", tc.input))
		assert.Equal(t, tc.err, err, fmt.Sprintf("Input: %s", tc.input))
	}
}

func TestGenLocalMAC(t *testing.T) {
	mac1 := GenLocalMAC("203.0.113.1")
	mac2 := GenLocalMAC("203.0.113.255")
	mac3 := GenLocalMAC("")
	assert.Equal(t, mac1, "02:42:cb:00:71:01")
	assert.Equal(t, mac2, "02:42:cb:00:71:ff")
	assert.Equal(t, mac3, "")
}

func TestReadIPFromStrInterface(t *testing.T) {
	testCases := []struct {
		input    interface{}
		expected net.IP
	}{
		{nil, nil},
		{"192.168.0.1", net.ParseIP("192.168.0.1")},
		{"invalid", nil},
		{123, nil},
	}

	for _, tc := range testCases {
		actual := ReadIPFromStrInterface(tc.input)
		assert.Equal(t, tc.expected, actual, fmt.Sprintf("Input: %v", tc.input))
	}
}
