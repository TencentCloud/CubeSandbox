// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

package enum HTTPRequestParser {
  package static func contentLength(from header: String) throws -> Int {
    let values = try header.split(separator: "\r\n").compactMap { line -> Int? in
      let components = line.split(
        separator: ":",
        maxSplits: 1,
        omittingEmptySubsequences: false
      )
      guard components.count == 2 else { return nil }
      guard
        components[0].trimmingCharacters(in: .whitespaces)
          .caseInsensitiveCompare("content-length") == .orderedSame
      else {
        return nil
      }

      let rawValue = components[1].trimmingCharacters(in: .whitespaces)
      guard let value = Int(rawValue), value >= 0 else {
        throw CubeVZError.invalidArguments("invalid Content-Length header")
      }
      return value
    }
    guard values.count <= 1 else {
      throw CubeVZError.invalidArguments("duplicate Content-Length headers")
    }
    return values.first ?? 0
  }
}
