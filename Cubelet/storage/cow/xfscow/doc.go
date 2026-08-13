// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package xfscow is the local XFS/reflink CoW Store implementation.
//
// The runtime type lives in package storage as [storage.XfsCow] (wired when
// storage_backend=cubecow). This package holds the canonical backend name and
// is the home for future xfscow-only helpers without pulling S3 concerns into
// the storage facade.
//
// A future S3-backed Store will live under storage/cow/s3 and implement the
// same [cow.Store] interface.
package xfscow

import "github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"

// Name is the Store.Name() value for the local XFS/reflink backend.
const Name = cow.NameXfsCow
