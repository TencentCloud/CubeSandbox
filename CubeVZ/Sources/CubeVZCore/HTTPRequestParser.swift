// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

package struct ParsedHTTPRequest: Equatable {
  package let method: String
  package let path: String
  package let body: Data
}

package enum HTTPRequestParser {
  package static func contentLength(from header: String) throws -> Int {
    let fields = header.split(separator: "\r\n").dropFirst()
    let transferEncodings = fields.compactMap { line -> String? in
      let components = line.split(
        separator: ":",
        maxSplits: 1,
        omittingEmptySubsequences: false
      )
      guard components.count == 2 else { return nil }
      guard
        components[0].trimmingCharacters(in: .whitespaces)
          .caseInsensitiveCompare("transfer-encoding") == .orderedSame
      else {
        return nil
      }
      return components[1].trimmingCharacters(in: .whitespaces)
    }
    guard transferEncodings.isEmpty else {
      throw CubeVZError.invalidArguments("Transfer-Encoding is not supported")
    }

    let values = try fields.compactMap { line -> Int? in
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
      guard
        !rawValue.isEmpty,
        rawValue.utf8.allSatisfy({ $0 >= UInt8(ascii: "0") && $0 <= UInt8(ascii: "9") }),
        let value = Int(rawValue)
      else {
        throw CubeVZError.invalidArguments("invalid Content-Length header")
      }
      return value
    }
    guard values.count <= 1 else {
      throw CubeVZError.invalidArguments("duplicate Content-Length headers")
    }
    return values.first ?? 0
  }

  package static func expectedRequestSize(
    in data: Data,
    maximumBytes: Int
  ) throws -> Int? {
    guard maximumBytes > 0 else {
      throw CubeVZError.invalidArguments("HTTP request limit must be positive")
    }
    guard data.count <= maximumBytes else {
      throw CubeVZError.invalidArguments("HTTP request is too large")
    }
    guard let headerRange = data.range(of: Data("\r\n\r\n".utf8)) else {
      return nil
    }
    guard
      let header = String(
        data: data.subdata(in: data.startIndex..<headerRange.lowerBound),
        encoding: .utf8
      )
    else {
      throw CubeVZError.invalidArguments("HTTP headers are not UTF-8")
    }
    let bodyLength = try contentLength(from: header)
    guard bodyLength <= maximumBytes - headerRange.upperBound else {
      throw CubeVZError.invalidArguments("HTTP request is too large")
    }
    return headerRange.upperBound + bodyLength
  }

  package static func parse(_ data: Data, maximumBytes: Int) throws -> ParsedHTTPRequest {
    guard
      let expectedSize = try expectedRequestSize(in: data, maximumBytes: maximumBytes),
      data.count >= expectedSize,
      let headerRange = data.range(of: Data("\r\n\r\n".utf8))
    else {
      throw CubeVZError.invalidArguments("incomplete HTTP request")
    }
    guard
      let header = String(
        data: data.subdata(in: data.startIndex..<headerRange.lowerBound),
        encoding: .utf8
      )
    else {
      throw CubeVZError.invalidArguments("HTTP headers are not UTF-8")
    }
    guard let requestLine = header.split(separator: "\r\n").first else {
      throw CubeVZError.invalidArguments("missing HTTP request line")
    }
    let components = requestLine.split(separator: " ", omittingEmptySubsequences: true)
    guard
      components.count == 3,
      !components[0].isEmpty,
      !components[1].isEmpty,
      components[2] == "HTTP/1.1" || components[2] == "HTTP/1.0"
    else {
      throw CubeVZError.invalidArguments("invalid HTTP request line")
    }
    return ParsedHTTPRequest(
      method: String(components[0]),
      path: String(components[1]),
      body: data.subdata(in: headerRange.upperBound..<expectedSize)
    )
  }
}
