// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation

package enum POSIXSocket {
  package static func makeLoopbackListener(
    port: UInt16,
    backlog: Int32 = 256,
    context: String
  ) throws -> Int32 {
    let descriptor = Darwin.socket(AF_INET, SOCK_STREAM, 0)
    guard descriptor >= 0 else {
      throw error(operation: "socket", context: context, code: errno)
    }

    var reuseAddress: Int32 = 1
    guard
      setsockopt(
        descriptor,
        SOL_SOCKET,
        SO_REUSEADDR,
        &reuseAddress,
        socklen_t(MemoryLayout.size(ofValue: reuseAddress))
      ) == 0
    else {
      let code = errno
      Darwin.close(descriptor)
      throw error(operation: "setsockopt", context: context, code: code)
    }

    var address = sockaddr_in()
    address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    address.sin_family = sa_family_t(AF_INET)
    address.sin_port = port.bigEndian
    address.sin_addr = in_addr(s_addr: inet_addr("127.0.0.1"))
    let bindResult = withUnsafePointer(to: &address) { pointer in
      pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
        Darwin.bind(descriptor, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
      }
    }
    guard bindResult == 0 else {
      let code = errno
      Darwin.close(descriptor)
      throw error(operation: "bind", context: context, code: code)
    }
    guard Darwin.listen(descriptor, backlog) == 0 else {
      let code = errno
      Darwin.close(descriptor)
      throw error(operation: "listen", context: context, code: code)
    }
    return descriptor
  }

  package static func read(
    into buffer: inout [UInt8],
    from descriptor: Int32,
    before deadline: UInt64,
    timeoutMessage: String,
    context: String
  ) throws -> Int {
    while true {
      let now = DispatchTime.now().uptimeNanoseconds
      guard now < deadline else {
        throw CubeVZError.runtime(timeoutMessage)
      }
      let remainingNanoseconds = deadline - now
      let timeoutMilliseconds = Int32(
        max(1, min(UInt64(Int32.max), (remainingNanoseconds + 999_999) / 1_000_000))
      )
      var pollDescriptor = pollfd(fd: descriptor, events: Int16(POLLIN), revents: 0)
      let ready = Darwin.poll(&pollDescriptor, 1, timeoutMilliseconds)
      if ready < 0 && errno == EINTR { continue }
      guard ready > 0 else {
        if ready == 0 {
          throw CubeVZError.runtime(timeoutMessage)
        }
        throw error(operation: "poll", context: context, code: errno)
      }

      var count: Int
      repeat {
        count = buffer.withUnsafeMutableBytes {
          Darwin.read(descriptor, $0.baseAddress, $0.count)
        }
      } while count < 0 && errno == EINTR
      return count
    }
  }

  @discardableResult
  package static func writeAll(_ data: Data, to descriptor: Int32) -> Bool {
    data.withUnsafeBytes { bytes in
      writeAll(bytes.baseAddress, count: bytes.count, to: descriptor)
    }
  }

  @discardableResult
  package static func writeAll(
    _ bytes: [UInt8],
    count: Int,
    to descriptor: Int32
  ) -> Bool {
    guard count >= 0, count <= bytes.count else { return false }
    return bytes.withUnsafeBytes { raw in
      writeAll(raw.baseAddress, count: count, to: descriptor)
    }
  }

  package static func close(_ descriptor: inout Int32) {
    guard descriptor >= 0 else { return }
    Darwin.close(descriptor)
    descriptor = -1
  }

  private static func writeAll(
    _ baseAddress: UnsafeRawPointer?,
    count: Int,
    to descriptor: Int32
  ) -> Bool {
    var offset = 0
    while offset < count {
      let written = Darwin.write(
        descriptor,
        baseAddress?.advanced(by: offset),
        count - offset
      )
      if written < 0 && errno == EINTR { continue }
      guard written > 0 else { return false }
      offset += written
    }
    return true
  }

  private static func error(operation: String, context: String, code: Int32) -> CubeVZError {
    .runtime("\(context) \(operation) failed: \(String(cString: strerror(code)))")
  }
}
