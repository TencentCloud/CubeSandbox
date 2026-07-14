// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

public enum CubeVZError: Error, CustomStringConvertible, Equatable {
  case invalidArguments(String)
  case invalidManifest(String)
  case unsupported(String)
  case filesystem(String)
  case runtime(String)

  public var description: String {
    switch self {
    case .invalidArguments(let message):
      return "invalid arguments: \(message)"
    case .invalidManifest(let message):
      return "invalid VM manifest: \(message)"
    case .unsupported(let message):
      return "unsupported: \(message)"
    case .filesystem(let message):
      return "filesystem error: \(message)"
    case .runtime(let message):
      return "runtime error: \(message)"
    }
  }
}
