// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

private struct DataPlaneRoute {
  let sandboxID: String
  let port: UInt32

  init(header: Data) throws {
    guard let text = String(data: header, encoding: .utf8) else {
      throw CubeVZError.invalidArguments("data-plane request headers are not UTF-8")
    }
    guard
      let hostLine = text.split(separator: "\r\n").first(where: {
        $0.lowercased().hasPrefix("host:")
      })
    else {
      throw CubeVZError.invalidArguments("data-plane request has no Host header")
    }

    let host = hostLine.dropFirst("host:".count).trimmingCharacters(in: .whitespaces)
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

final class DataPlaneProxy: @unchecked Sendable {
  private static let maximumHeaderBytes = 64 * 1_024

  private let port: UInt16
  private let manager: SandboxManager
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-data-plane",
    qos: .userInitiated,
    attributes: .concurrent
  )
  private var listenDescriptor: Int32 = -1

  init(port: UInt16, manager: SandboxManager) {
    self.port = port
    self.manager = manager
  }

  func start() throws {
    signal(SIGPIPE, SIG_IGN)
    let descriptor = Darwin.socket(AF_INET, SOCK_STREAM, 0)
    guard descriptor >= 0 else { throw posixError("socket") }

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
      Darwin.close(descriptor)
      throw posixError("setsockopt")
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
      Darwin.close(descriptor)
      throw posixError("bind")
    }
    guard Darwin.listen(descriptor, 256) == 0 else {
      Darwin.close(descriptor)
      throw posixError("listen")
    }
    listenDescriptor = descriptor
    workerQueue.async { [self] in acceptLoop() }
  }

  private func acceptLoop() {
    while true {
      let client = Darwin.accept(listenDescriptor, nil, nil)
      if client < 0 {
        if errno == EINTR { continue }
        return
      }
      workerQueue.async { [self] in route(client: client) }
    }
  }

  private func route(client: Int32) {
    do {
      let prefix = try readHeaderPrefix(from: client)
      let route = try DataPlaneRoute(header: prefix)
      Task { @MainActor [self, manager] in
        do {
          let guest = try await manager.openDataPlane(
            sandboxID: route.sandboxID,
            guestPort: route.port
          )
          workerQueue.async { [self] in relay(client: client, guest: guest, prefix: prefix) }
        } catch {
          sendError(status: 502, message: error.localizedDescription, to: client)
        }
      }
    } catch {
      sendError(status: 400, message: error.localizedDescription, to: client)
    }
  }

  // Read only enough bytes to route the connection. The exact bytes, including
  // any body bytes received in the same packet, are forwarded unchanged.
  private func readHeaderPrefix(from descriptor: Int32) throws -> Data {
    var data = Data()
    let terminator = Data("\r\n\r\n".utf8)
    while data.count < Self.maximumHeaderBytes {
      var buffer = [UInt8](repeating: 0, count: 4_096)
      let count = buffer.withUnsafeMutableBytes {
        Darwin.read(descriptor, $0.baseAddress, $0.count)
      }
      guard count > 0 else {
        throw CubeVZError.runtime("data-plane client disconnected")
      }
      data.append(contentsOf: buffer.prefix(count))
      if data.range(of: terminator) != nil { return data }
    }
    throw CubeVZError.invalidArguments("data-plane request headers are too large")
  }

  private func relay(client: Int32, guest: VMStreamConnection, prefix: Data) {
    let guestDescriptor = guest.fileDescriptor
    guard writeAll(prefix, to: guestDescriptor) else {
      guest.close()
      Darwin.close(client)
      return
    }

    var clientReadable = true
    var guestReadable = true
    var buffer = [UInt8](repeating: 0, count: 64 * 1_024)
    while clientReadable || guestReadable {
      var descriptors = [
        pollfd(fd: client, events: clientReadable ? Int16(POLLIN) : 0, revents: 0),
        pollfd(fd: guestDescriptor, events: guestReadable ? Int16(POLLIN) : 0, revents: 0),
      ]
      let ready = Darwin.poll(&descriptors, 2, -1)
      if ready < 0 {
        if errno == EINTR { continue }
        break
      }
      if clientReadable && descriptors[0].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        let count = buffer.withUnsafeMutableBytes {
          Darwin.read(client, $0.baseAddress, $0.count)
        }
        if count <= 0 || !writeAll(buffer.prefix(max(count, 0)), to: guestDescriptor) {
          clientReadable = false
          Darwin.shutdown(guestDescriptor, SHUT_WR)
        }
      }
      if guestReadable && descriptors[1].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        let count = buffer.withUnsafeMutableBytes {
          Darwin.read(guestDescriptor, $0.baseAddress, $0.count)
        }
        if count <= 0 || !writeAll(buffer.prefix(max(count, 0)), to: client) {
          guestReadable = false
          Darwin.shutdown(client, SHUT_WR)
        }
      }
    }
    guest.close()
    Darwin.close(client)
  }

  private func writeAll<C: Collection>(_ bytes: C, to descriptor: Int32) -> Bool
  where C.Element == UInt8 {
    let data = Data(bytes)
    return data.withUnsafeBytes { raw in
      var offset = 0
      while offset < raw.count {
        let count = Darwin.write(
          descriptor,
          raw.baseAddress?.advanced(by: offset),
          raw.count - offset
        )
        if count < 0 && errno == EINTR { continue }
        guard count > 0 else { return false }
        offset += count
      }
      return true
    }
  }

  private func sendError(status: Int, message: String, to descriptor: Int32) {
    let body = (try? JSONSerialization.data(withJSONObject: ["error": message])) ?? Data()
    let response = Data(
      "HTTP/1.1 \(status) Error\r\nContent-Type: application/json\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
        .utf8
    ) + body
    _ = writeAll(response, to: descriptor)
    Darwin.close(descriptor)
  }

  private func posixError(_ operation: String) -> CubeVZError {
    .runtime("data-plane \(operation) failed: \(String(cString: strerror(errno)))")
  }
}
