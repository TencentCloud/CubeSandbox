// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package inner provides the inner services of cube-master
package inner

import (
	"path/filepath"
)

const (
	innerURI         = "/internal"
	NodeAction       = "/node"
	FakeCreateAction = "/fake_create"
	StateWs          = "/ws"
	StateQuery       = "/query"
)

func InnerURI() string {
	return innerURI
}

func actionURI(uri string) string {
	return filepath.Clean(filepath.Join(innerURI, uri))
}
