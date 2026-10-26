// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3 holds the canonical backend name for the S3 CoW Store.
//
// The runtime type lives in package storage as [storage.S3Cow], bound to the
// cubecow kind=s3 handle. Cross-node Pause／Create use Upload／Fetch／Status
// on that Store. Do not fold S3 export logic into [storage.XfsCow].
package s3

import "github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"

// Name is the Store.Name() value for the S3-backed backend.
const Name = cow.NameS3
