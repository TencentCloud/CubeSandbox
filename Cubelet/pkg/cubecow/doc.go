// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubecow provides Go bindings for the cubecow C FFI library.
//
// cubecow is a reflink-only copy-on-write storage engine. Volumes are
// regular files on a reflink-capable filesystem (XFS or Btrfs) and
// snapshots are O(1) FICLONE-based clones of those files.
//
// Thread model:
//   - cubecow_last_error() is thread-local on the Rust FFI side.
//   - Every FFI wrapper in this package locks the current OS thread before calling
//     into C and copies the error string into Go memory before unlocking.
//   - Callers may invoke the SDK from arbitrary goroutines; the package handles
//     the thread-local last_error constraint internally.
//
// Lifecycle:
//   - A Cubelet process should hold exactly one *Engine instance.
//   - Initialization supports either a TOML config path (Init / InitWithoutLogging)
//     or a JSON config string (InitFromJSON / InitWithoutLoggingFromJSON).
//   - Repeated Init calls are not supported as a normal pattern; storage/plugin.go
//     owns process-wide initialization and fail-fast behavior.
//   - Close is idempotent. If the process exits unexpectedly, cubecow's underlying
//     flock lock is released by the kernel.
//
// Error semantics:
//   - This package returns CowError with SemanticCode and Action.
//   - The SDK does not swallow NOT_FOUND or ALREADY_EXISTS automatically.
//   - Higher business layers decide whether a given error is retriable or can be
//     treated as an idempotent success on a specific workflow path.
//
// Object naming conventions are decided by higher layers. Common patterns:
//   - tpl-<snapshotID>-rootfs
//   - tpl-<snapshotID>-memory
//   - sb-<sandboxID>-rootfs-gen<N>
//   - tpl-<templateID>-build-rootfs
//
// Device paths returned by cubecow are stable regular file paths on the
// reflink filesystem; persisted device_path values can be reused after
// restart without re-resolution (refresh on startup is still recommended
// to detect manual filesystem changes).
package cubecow
