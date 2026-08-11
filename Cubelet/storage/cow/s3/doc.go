// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3 is reserved for a future S3-backed CoW Store implementation.
//
// It will satisfy [cow.Store] with the same object naming and kind semantics as
// xfscow, while Resolve/Delete talk to remote snapshot objects. Not implemented
// yet — see package xfscow for the current backend.
package s3

import "github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"

// Name is the Store.Name() value reserved for the S3-backed backend.
const Name = cow.NameS3
