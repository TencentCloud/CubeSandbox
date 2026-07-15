// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation

package enum SocketLineReader {
  package static func readLine(
    from descriptor: Int32,
    maximumBytes: Int = 256,
    timeoutMilliseconds: Int32 = 2_000
  ) throws -> String {
    guard maximumBytes > 0, timeoutMilliseconds > 0 else {
      throw CubeVZError.invalidArguments("socket line limits must be positive")
    }

    let timeoutNanoseconds = UInt64(timeoutMilliseconds) * 1_000_000
    let deadline = DispatchTime.now().uptimeNanoseconds + timeoutNanoseconds
    var response: [UInt8] = []

    while response.count < maximumBytes {
      let now = DispatchTime.now().uptimeNanoseconds
      guard now < deadline else {
        throw CubeVZError.runtime("socket response timed out")
      }
      let remainingNanoseconds = deadline - now
      let remainingMilliseconds = max(
        1,
        min(
          UInt64(Int32.max),
          (remainingNanoseconds + 999_999) / 1_000_000
        )
      )
      var pollDescriptor = pollfd(fd: descriptor, events: Int16(POLLIN), revents: 0)
      let ready = Darwin.poll(&pollDescriptor, 1, Int32(remainingMilliseconds))
      if ready < 0 && errno == EINTR { continue }
      guard ready > 0 else {
        if ready == 0 {
          throw CubeVZError.runtime("socket response timed out")
        }
        throw CubeVZError.runtime(
          "socket poll failed: \(String(cString: strerror(errno)))"
        )
      }

      var buffer = [UInt8](
        repeating: 0,
        count: min(64, maximumBytes - response.count)
      )
      var count: Int
      repeat {
        count = buffer.withUnsafeMutableBytes {
          Darwin.read(descriptor, $0.baseAddress, $0.count)
        }
      } while count < 0 && errno == EINTR
      guard count > 0 else {
        throw CubeVZError.runtime("socket closed before a complete response")
      }
      response.append(contentsOf: buffer.prefix(count))

      if let newline = response.firstIndex(of: UInt8(ascii: "\n")) {
        return String(decoding: response[...newline], as: UTF8.self)
      }
    }

    throw CubeVZError.runtime("socket response exceeds \(maximumBytes) bytes")
  }
}
