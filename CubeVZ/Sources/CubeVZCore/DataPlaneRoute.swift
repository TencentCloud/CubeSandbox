// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

package struct DataPlaneRoute: Equatable {
  package let sandboxID: String
  package let port: UInt32

  package init(header: Data) throws {
    guard let headerEnd = header.range(of: Data("\r\n\r\n".utf8)) else {
      throw CubeVZError.invalidArguments("data-plane request headers are incomplete")
    }
    guard
      let text = String(
        data: header.subdata(in: header.startIndex..<headerEnd.lowerBound),
        encoding: .utf8
      )
    else {
      throw CubeVZError.invalidArguments("data-plane request headers are not UTF-8")
    }

    let hostValues = text.split(separator: "\r\n").dropFirst().compactMap { line -> String? in
      let components = line.split(
        separator: ":",
        maxSplits: 1,
        omittingEmptySubsequences: false
      )
      guard components.count == 2 else { return nil }
      guard
        components[0].trimmingCharacters(in: .whitespaces)
          .caseInsensitiveCompare("host") == .orderedSame
      else {
        return nil
      }
      return components[1].trimmingCharacters(in: .whitespaces)
    }
    guard hostValues.count == 1, let host = hostValues.first, !host.isEmpty else {
      throw CubeVZError.invalidArguments("data-plane request must have one Host header")
    }

    let firstLabel = host.split(separator: ".", maxSplits: 1).first.map(String.init) ?? host
    guard let separator = firstLabel.firstIndex(of: "-") else {
      throw CubeVZError.invalidArguments("invalid data-plane Host header")
    }
    let rawPort = firstLabel[..<separator]
    let rawSandboxID = firstLabel[firstLabel.index(after: separator)...]
    guard let port = UInt32(rawPort), port > 0, port <= UInt32(UInt16.max) else {
      throw CubeVZError.invalidArguments("invalid data-plane port")
    }
    guard !rawSandboxID.isEmpty else {
      throw CubeVZError.invalidArguments("invalid data-plane sandbox ID")
    }
    self.port = port
    sandboxID = String(rawSandboxID)
  }
}
