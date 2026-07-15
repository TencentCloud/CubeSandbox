// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

final class DataPlaneProxy: @unchecked Sendable {
  private static let headerReadTimeoutNanoseconds: UInt64 = 5_000_000_000
  private static let relayIdleTimeoutMilliseconds: Int32 = 60_000
  private static let maximumConnections = 128
  private static let maximumHeaderBytes = 64 * 1_024

  private let port: UInt16
  private let manager: SandboxManager
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-data-plane",
    qos: .userInitiated,
    attributes: .concurrent
  )
  private var listenDescriptor: Int32 = -1
  private let connectionSlots = DispatchSemaphore(value: maximumConnections)

  init(port: UInt16, manager: SandboxManager) {
    self.port = port
    self.manager = manager
  }

  deinit {
    POSIXSocket.close(&listenDescriptor)
  }

  func start() throws {
    signal(SIGPIPE, SIG_IGN)
    listenDescriptor = try POSIXSocket.makeLoopbackListener(
      port: port,
      context: "data-plane"
    )
    workerQueue.async { [self] in acceptLoop() }
  }

  private func acceptLoop() {
    while true {
      let client = Darwin.accept(listenDescriptor, nil, nil)
      if client < 0 {
        if errno == EINTR { continue }
        if [ECONNABORTED, EMFILE, ENFILE, ENOBUFS, ENOMEM].contains(errno) {
          usleep(50_000)
          continue
        }
        return
      }
      guard connectionSlots.wait(timeout: .now()) == .success else {
        sendError(status: 503, message: "data-plane capacity reached", to: client)
        continue
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
          Self.log(error, context: "open data-plane route")
          sendError(status: 502, message: "sandbox data plane unavailable", to: client)
          connectionSlots.signal()
        }
      }
    } catch {
      Self.log(error, context: "parse data-plane request")
      sendError(status: 400, message: "invalid data-plane request", to: client)
      connectionSlots.signal()
    }
  }

  // Read only enough bytes to route the connection. The exact bytes, including
  // any body bytes received in the same packet, are forwarded unchanged.
  private func readHeaderPrefix(from descriptor: Int32) throws -> Data {
    var data = Data()
    let terminator = Data("\r\n\r\n".utf8)
    let deadline = DispatchTime.now().uptimeNanoseconds + Self.headerReadTimeoutNanoseconds
    while data.count < Self.maximumHeaderBytes {
      var buffer = [UInt8](repeating: 0, count: 4_096)
      let count = try POSIXSocket.read(
        into: &buffer,
        from: descriptor,
        before: deadline,
        timeoutMessage: "data-plane request headers timed out",
        context: "data-plane"
      )
      guard count > 0 else {
        throw CubeVZError.runtime("data-plane client disconnected")
      }
      data.append(contentsOf: buffer.prefix(count))
      if let headerEnd = data.range(of: terminator) {
        guard headerEnd.lowerBound <= Self.maximumHeaderBytes else {
          throw CubeVZError.invalidArguments("data-plane request headers are too large")
        }
        return data
      }
    }
    throw CubeVZError.invalidArguments("data-plane request headers are too large")
  }

  private func relay(client: Int32, guest: VMStreamConnection, prefix: Data) {
    let guestDescriptor = guest.fileDescriptor
    defer {
      guest.close()
      Darwin.close(client)
      connectionSlots.signal()
    }
    guard POSIXSocket.writeAll(prefix, to: guestDescriptor) else {
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
      let ready = Darwin.poll(&descriptors, 2, Self.relayIdleTimeoutMilliseconds)
      if ready < 0 {
        if errno == EINTR { continue }
        break
      }
      if ready == 0 { break }
      if clientReadable && descriptors[0].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        var count: Int
        repeat {
          count = buffer.withUnsafeMutableBytes {
            Darwin.read(client, $0.baseAddress, $0.count)
          }
        } while count < 0 && errno == EINTR
        if count <= 0 || !POSIXSocket.writeAll(buffer, count: count, to: guestDescriptor) {
          clientReadable = false
          Darwin.shutdown(guestDescriptor, SHUT_WR)
        }
      }
      if guestReadable && descriptors[1].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        var count: Int
        repeat {
          count = buffer.withUnsafeMutableBytes {
            Darwin.read(guestDescriptor, $0.baseAddress, $0.count)
          }
        } while count < 0 && errno == EINTR
        if count <= 0 || !POSIXSocket.writeAll(buffer, count: count, to: client) {
          guestReadable = false
          Darwin.shutdown(client, SHUT_WR)
        }
      }
    }
  }

  private func sendError(status: Int, message: String, to descriptor: Int32) {
    let body = (try? JSONSerialization.data(withJSONObject: ["error": message])) ?? Data()
    let reason: String
    switch status {
    case 400: reason = "Bad Request"
    case 502: reason = "Bad Gateway"
    case 503: reason = "Service Unavailable"
    default: reason = "Error"
    }
    let header =
      "HTTP/1.1 \(status) \(reason)\r\n"
      + "Content-Type: application/json\r\n"
      + "Content-Length: \(body.count)\r\n"
      + "Connection: close\r\n\r\n"
    _ = POSIXSocket.writeAll(Data(header.utf8), to: descriptor)
    _ = POSIXSocket.writeAll(body, to: descriptor)
    Darwin.close(descriptor)
  }

  private static func log(_ error: Error, context: String) {
    FileHandle.standardError.write(
      Data("cube-vz-api: \(context): \(error.localizedDescription)\n".utf8)
    )
  }
}
