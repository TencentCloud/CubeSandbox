// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

private struct HTTPRequest {
  let method: String
  let path: String
  let headers: [String: String]
  let body: Data

  var routePath: String {
    path.split(separator: "?", maxSplits: 1).first.map(String.init) ?? path
  }

  var queryItems: [String: String] {
    guard let query = path.split(separator: "?", maxSplits: 1).dropFirst().first else {
      return [:]
    }
    var items: [String: String] = [:]
    for component in query.split(separator: "&") {
      let parts = component.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
      let key = String(parts[0]).removingPercentEncoding ?? String(parts[0])
      let rawValue = parts.count == 2 ? String(parts[1]) : ""
      items[key] = rawValue.removingPercentEncoding ?? rawValue
    }
    return items
  }

  var dataPlaneSandboxID: String? {
    guard let host = headers["host"]?.lowercased() else { return nil }
    let prefix = host.split(separator: ".", maxSplits: 1).first.map(String.init) ?? host
    guard prefix.hasPrefix("49983-") else { return nil }
    let sandboxID = String(prefix.dropFirst("49983-".count))
    return sandboxID.isEmpty ? nil : sandboxID
  }

  var trafficAccessToken: String? {
    headers["e2b-traffic-access-token"] ?? headers["cube-traffic-access-token"]
  }

  func envdRequest() -> Data {
    var request = "\(method) \(path) HTTP/1.1\r\n"
    for (name, value) in headers.sorted(by: { $0.key < $1.key }) {
      if name == "host" || name == "connection" || name == "proxy-connection" {
        continue
      }
      request += "\(name): \(value)\r\n"
    }
    request += "Host: 127.0.0.1:49983\r\nConnection: close\r\n\r\n"
    return Data(request.utf8) + body
  }

  func forwardedRequest() -> Data {
    var request = "\(method) \(path) HTTP/1.1\r\n"
    for (name, value) in headers.sorted(by: { $0.key < $1.key }) {
      if name == "connection" || name == "proxy-connection" { continue }
      request += "\(name): \(value)\r\n"
    }
    request += "Connection: close\r\n\r\n"
    return Data(request.utf8) + body
  }
}

private struct TimeoutRequest: Decodable {
  let timeout: Int?
}

private struct SnapshotRequest: Decodable {
  let name: String?
}

private struct RollbackRequest: Decodable {
  let snapshotID: String
}

final class HTTPServer: @unchecked Sendable {
  private let bindAddress: String
  private let port: UInt16
  private let manager: SandboxManager
  private let scheduler: ClusterScheduler
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-api",
    qos: .userInitiated,
    attributes: .concurrent
  )
  private var listenDescriptor: Int32 = -1

  init(bindAddress: String, port: UInt16, manager: SandboxManager, scheduler: ClusterScheduler) {
    self.bindAddress = bindAddress
    self.port = port
    self.manager = manager
    self.scheduler = scheduler
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
    guard bindAddress.withCString({ inet_pton(AF_INET, $0, &address.sin_addr) }) == 1 else {
      Darwin.close(descriptor)
      throw CubeVZError.invalidArguments("--bind-address must be an IPv4 address")
    }
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

    workerQueue.async { [self] in
      acceptLoop()
    }
  }

  private func acceptLoop() {
    while true {
      let client = Darwin.accept(listenDescriptor, nil, nil)
      if client < 0 {
        if errno == EINTR { continue }
        return
      }
      workerQueue.async { [self] in
        handle(client: client)
      }
    }
  }

  private func handle(client: Int32) {
    do {
      let request = try readRequest(from: client)
      if let sandboxID = request.dataPlaneSandboxID {
        Task { @MainActor [manager, scheduler] in
          do {
            if let remoteURL = scheduler.remoteURL(for: sandboxID) {
              workerQueue.async { [self] in
                proxyRemote(request: request, client: client, remoteURL: remoteURL)
              }
              return
            }
            let connection = try await manager.openDataPlane(
              sandboxID: sandboxID,
              trafficAccessToken: request.trafficAccessToken
            )
            workerQueue.async { [self] in
              proxy(request: request, client: client, connection: connection)
            }
          } catch {
            sendError(status: 502, message: error.localizedDescription, to: client)
          }
        }
        return
      }
      Task { @MainActor [manager, scheduler] in
        do {
          let routePath = request.routePath
          if let sandboxID = Self.anySandboxID(from: routePath),
            let response = try await scheduler.forwardControl(
              method: request.method,
              path: request.path,
              headers: request.headers,
              body: request.body,
              sandboxID: sandboxID
            )
          {
            send(
              status: response.status,
              body: response.body,
              headers: response.headers,
              to: client
            )
            return
          }
          switch (request.method, routePath) {
          case ("GET", "/health"):
            send(status: 200, body: Data("{\"status\":\"ok\"}".utf8), to: client)

          case ("GET", "/sandboxes"), ("GET", "/v2/sandboxes"):
            send(status: 200, body: try JSONEncoder().encode(manager.list()), to: client)

          case ("POST", "/sandboxes"):
            let createRequest = try JSONDecoder().decode(
              CreateSandboxRequest.self,
              from: request.body
            )
            let response = try await scheduler.create(request: createRequest, rawBody: request.body)
            send(
              status: response.status,
              body: response.body,
              headers: response.headers,
              to: client
            )

          case ("POST", "/cluster/nodes/register"):
            let registration = try decode(ClusterRegistrationRequest.self, from: request.body)
            try scheduler.register(registration)
            send(status: 204, body: Data(), to: client)

          case ("GET", "/nodes"):
            send(
              status: 200,
              body: try JSONEncoder().encode(scheduler.nodeResponses()),
              to: client
            )

          case ("GET", "/cluster/overview"):
            send(
              status: 200,
              body: try JSONEncoder().encode(scheduler.overview()),
              to: client
            )

          case ("GET", let path) where path.hasPrefix("/nodes/"):
            let nodeID = String(path.dropFirst("/nodes/".count))
            if let response = scheduler.nodeResponse(nodeID: nodeID) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "node not found", to: client)
            }

          case ("GET", let path) where Self.sandboxID(from: path, suffix: nil) != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: nil)!
            if let response = manager.get(sandboxID: sandboxID) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("DELETE", let path) where Self.sandboxID(from: path, suffix: nil) != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: nil)!
            if try await manager.delete(sandboxID: sandboxID) {
              send(status: 204, body: Data(), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "pause") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "pause")!
            if try await manager.pause(sandboxID: sandboxID) {
              send(status: 204, body: Data(), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "resume") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "resume")!
            let body = try decode(TimeoutRequest.self, from: request.body)
            if let response = try await manager.resume(
              sandboxID: sandboxID,
              timeout: body.timeout
            ) {
              send(status: 201, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "connect") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "connect")!
            let body = try decode(TimeoutRequest.self, from: request.body)
            if let response = try await manager.connect(
              sandboxID: sandboxID,
              timeout: body.timeout
            ) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "timeout") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "timeout")!
            let body = try decode(TimeoutRequest.self, from: request.body)
            guard let timeout = body.timeout else {
              sendError(status: 400, message: "timeout is required", to: client)
              return
            }
            if try manager.setTimeout(sandboxID: sandboxID, timeout: timeout) {
              send(status: 204, body: Data(), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "snapshots") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "snapshots")!
            let body = try decode(SnapshotRequest.self, from: request.body)
            if let response = try await manager.createSnapshot(
              sandboxID: sandboxID,
              name: body.name
            ) {
              send(status: 201, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("GET", "/snapshots"):
            let response = manager.listSnapshots(
              originSandboxID: request.queryItems["sandboxID"]
            )
            send(status: 200, body: try JSONEncoder().encode(response), to: client)

          case ("DELETE", let path) where path.hasPrefix("/templates/"):
            let snapshotID = String(path.dropFirst("/templates/".count))
            if try manager.deleteSnapshot(snapshotID: snapshotID) {
              let response = try JSONSerialization.data(withJSONObject: [
                "templateID": snapshotID,
                "operationID": UUID().uuidString.lowercased(),
                "status": "success",
              ])
              send(status: 200, body: response, to: client)
            } else {
              sendError(status: 404, message: "template not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "rollback") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "rollback")!
            let body = try decode(RollbackRequest.self, from: request.body)
            if let response = try await manager.rollback(
              sandboxID: sandboxID,
              snapshotID: body.snapshotID
            ) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          default:
            sendError(status: 404, message: "route not found", to: client)
          }
        } catch {
          sendError(status: 500, message: error.localizedDescription, to: client)
        }
      }
    } catch {
      sendError(status: 400, message: error.localizedDescription, to: client)
    }
  }

  private static func sandboxID(from path: String, suffix: String?) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count >= 2, components[0] == "sandboxes" else { return nil }
    if let suffix {
      guard components.count == 3, components[2] == Substring(suffix) else { return nil }
    } else if components.count != 2 {
      return nil
    }
    let sandboxID = String(components[1])
    return sandboxID.isEmpty ? nil : sandboxID
  }

  private static func anySandboxID(from path: String) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count >= 2, components[0] == "sandboxes" else { return nil }
    return String(components[1])
  }

  private func decode<T: Decodable>(_ type: T.Type, from body: Data) throws -> T {
    try JSONDecoder().decode(type, from: body.isEmpty ? Data("{}".utf8) : body)
  }

  private func readRequest(from descriptor: Int32) throws -> HTTPRequest {
    var data = Data()
    var expectedSize: Int?

    while data.count < 67_108_864 {
      var buffer = [UInt8](repeating: 0, count: 4096)
      let count = buffer.withUnsafeMutableBytes {
        Darwin.read(descriptor, $0.baseAddress, $0.count)
      }
      guard count > 0 else { throw CubeVZError.runtime("HTTP client disconnected") }
      data.append(contentsOf: buffer.prefix(count))

      if expectedSize == nil,
        let headerRange = data.range(of: Data("\r\n\r\n".utf8))
      {
        let header = String(decoding: data[..<headerRange.lowerBound], as: UTF8.self)
        let contentLength =
          header.split(separator: "\r\n").first { line in
            line.lowercased().hasPrefix("content-length:")
          }.flatMap {
            Int($0.split(separator: ":", maxSplits: 1)[1].trimmingCharacters(in: .whitespaces))
          } ?? 0
        expectedSize = headerRange.upperBound + contentLength
      }
      if let expectedSize, data.count >= expectedSize { break }
    }

    guard let headerRange = data.range(of: Data("\r\n\r\n".utf8)) else {
      throw CubeVZError.invalidArguments("invalid HTTP headers")
    }
    let header = String(decoding: data[..<headerRange.lowerBound], as: UTF8.self)
    guard let requestLine = header.split(separator: "\r\n").first else {
      throw CubeVZError.invalidArguments("missing HTTP request line")
    }
    let components = requestLine.split(separator: " ")
    guard components.count >= 2 else {
      throw CubeVZError.invalidArguments("invalid HTTP request line")
    }
    let bodyEnd = min(expectedSize ?? data.count, data.count)
    var headers: [String: String] = [:]
    for line in header.split(separator: "\r\n").dropFirst() {
      let parts = line.split(separator: ":", maxSplits: 1)
      guard parts.count == 2 else { continue }
      headers[String(parts[0]).lowercased()] = parts[1].trimmingCharacters(in: .whitespaces)
    }
    return HTTPRequest(
      method: String(components[0]),
      path: String(components[1]),
      headers: headers,
      body: data.subdata(in: headerRange.upperBound..<bodyEnd)
    )
  }

  private func proxy(
    request: HTTPRequest,
    client: Int32,
    connection: VMStreamConnection
  ) {
    let guest = connection.fileDescriptor
    guard writeAll(request.envdRequest(), to: guest) else {
      connection.close()
      Darwin.close(client)
      return
    }

    var clientOpen = true
    var buffer = [UInt8](repeating: 0, count: 65_536)
    while true {
      var descriptors = [
        pollfd(fd: client, events: clientOpen ? Int16(POLLIN) : 0, revents: 0),
        pollfd(fd: guest, events: Int16(POLLIN), revents: 0),
      ]
      let result = Darwin.poll(&descriptors, 2, -1)
      if result < 0 {
        if errno == EINTR { continue }
        break
      }
      if clientOpen, descriptors[0].revents & (Int16(POLLIN | POLLHUP | POLLERR)) != 0 {
        let count = buffer.withUnsafeMutableBytes {
          Darwin.read(client, $0.baseAddress, $0.count)
        }
        if count <= 0 {
          clientOpen = false
          Darwin.shutdown(guest, SHUT_WR)
        } else if !writeAll(Data(buffer.prefix(count)), to: guest) {
          break
        }
      }
      if descriptors[1].revents & (Int16(POLLIN | POLLHUP | POLLERR)) != 0 {
        let count = buffer.withUnsafeMutableBytes {
          Darwin.read(guest, $0.baseAddress, $0.count)
        }
        if count <= 0 || !writeAll(Data(buffer.prefix(max(count, 0))), to: client) {
          break
        }
      }
    }
    connection.close()
    Darwin.close(client)
  }

  private func proxyRemote(request: HTTPRequest, client: Int32, remoteURL: URL) {
    guard remoteURL.scheme == "http", let host = remoteURL.host else {
      sendError(status: 502, message: "remote node URL is invalid", to: client)
      return
    }
    let remote = Darwin.socket(AF_INET, SOCK_STREAM, 0)
    guard remote >= 0 else {
      sendError(status: 502, message: "cannot create remote node socket", to: client)
      return
    }
    var address = sockaddr_in()
    address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    address.sin_family = sa_family_t(AF_INET)
    address.sin_port = UInt16(remoteURL.port ?? 80).bigEndian
    guard host.withCString({ inet_pton(AF_INET, $0, &address.sin_addr) }) == 1 else {
      Darwin.close(remote)
      sendError(status: 502, message: "remote node host must be an IPv4 address", to: client)
      return
    }
    let connected = withUnsafePointer(to: &address) { pointer in
      pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
        Darwin.connect(remote, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
      }
    }
    guard connected == 0, writeAll(request.forwardedRequest(), to: remote) else {
      Darwin.close(remote)
      sendError(status: 502, message: "cannot connect to remote node", to: client)
      return
    }
    relay(left: client, right: remote)
    Darwin.close(remote)
    Darwin.close(client)
  }

  private func relay(left: Int32, right: Int32) {
    var leftOpen = true
    var buffer = [UInt8](repeating: 0, count: 65_536)
    while true {
      var descriptors = [
        pollfd(fd: left, events: leftOpen ? Int16(POLLIN) : 0, revents: 0),
        pollfd(fd: right, events: Int16(POLLIN), revents: 0),
      ]
      let result = Darwin.poll(&descriptors, 2, -1)
      if result < 0 {
        if errno == EINTR { continue }
        break
      }
      if leftOpen, descriptors[0].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        let count = buffer.withUnsafeMutableBytes { Darwin.read(left, $0.baseAddress, $0.count) }
        if count <= 0 {
          leftOpen = false
          Darwin.shutdown(right, SHUT_WR)
        } else if !writeAll(Data(buffer.prefix(count)), to: right) {
          break
        }
      }
      if descriptors[1].revents & Int16(POLLIN | POLLHUP | POLLERR) != 0 {
        let count = buffer.withUnsafeMutableBytes { Darwin.read(right, $0.baseAddress, $0.count) }
        if count <= 0 || !writeAll(Data(buffer.prefix(max(count, 0))), to: left) { break }
      }
    }
  }

  private func send(
    status: Int,
    body: Data,
    headers: [String: String] = [:],
    to descriptor: Int32
  ) {
    let reason: String
    switch status {
    case 200: reason = "OK"
    case 201: reason = "Created"
    case 204: reason = "No Content"
    case 400: reason = "Bad Request"
    case 403: reason = "Forbidden"
    case 404: reason = "Not Found"
    case 409: reason = "Conflict"
    case 502: reason = "Bad Gateway"
    default: reason = "Internal Server Error"
    }
    var headerText = "HTTP/1.1 \(status) \(reason)\r\n"
    headerText += "Content-Type: \(headers["content-type"] ?? "application/json")\r\n"
    for (name, value) in headers
    where !["content-type", "content-length", "connection", "transfer-encoding"].contains(
      name.lowercased()
    ) {
      headerText += "\(name): \(value)\r\n"
    }
    headerText += "Content-Length: \(body.count)\r\nConnection: close\r\n\r\n"
    let header = Data(headerText.utf8)
    _ = writeAll(header + body, to: descriptor)
    Darwin.close(descriptor)
  }

  private func sendError(status: Int, message: String, to descriptor: Int32) {
    let body = (try? JSONSerialization.data(withJSONObject: ["error": message])) ?? Data()
    send(status: status, body: body, to: descriptor)
  }

  private func writeAll(_ data: Data, to descriptor: Int32) -> Bool {
    data.withUnsafeBytes { bytes -> Bool in
      var offset = 0
      while offset < bytes.count {
        let written = Darwin.write(
          descriptor,
          bytes.baseAddress?.advanced(by: offset),
          bytes.count - offset
        )
        if written <= 0 { return false }
        offset += written
      }
      return true
    }
  }

  private func posixError(_ operation: String) -> CubeVZError {
    .runtime("\(operation) failed: \(String(cString: strerror(errno)))")
  }
}
