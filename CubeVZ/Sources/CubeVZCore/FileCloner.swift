// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation

public enum FileCloner {
  public static func clone(
    from source: URL,
    to destination: URL,
    allowFullCopy: Bool
  ) throws {
    guard source.isFileURL, destination.isFileURL else {
      throw CubeVZError.filesystem("source and destination must be local file URLs")
    }
    guard FileManager.default.fileExists(atPath: source.path) else {
      throw CubeVZError.filesystem("source does not exist: \(source.path)")
    }

    let result = source.path.withCString { sourcePath in
      destination.path.withCString { destinationPath in
        clonefile(sourcePath, destinationPath, 0)
      }
    }
    if result == 0 {
      return
    }

    let cloneError = POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    guard allowFullCopy else {
      throw CubeVZError.filesystem(
        "APFS clonefile failed for \(source.path): \(cloneError). "
          + "Use --allow-full-copy only if losing copy-on-write storage is acceptable."
      )
    }

    do {
      try FileManager.default.copyItem(at: source, to: destination)
    } catch {
      throw CubeVZError.filesystem(
        "clonefile failed (\(cloneError)) and full copy failed: \(error)"
      )
    }
  }
}
