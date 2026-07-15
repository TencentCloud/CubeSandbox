// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

private struct HTTPRequest {
  let method: String
  let path: String
  let body: Data
}

final class HTTPServer: @unchecked Sendable {
  private static let maximumRequestBytes = 1_048_576

  private let port: UInt16
  private let manager: SandboxManager
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-api",
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
      Task { @MainActor [manager] in
        do {
          switch (request.method, request.path) {
          case ("GET", "/health"):
            send(status: 200, body: Data("{\"status\":\"ok\"}".utf8), to: client)

          case ("POST", "/sandboxes"):
            let createRequest = try JSONDecoder().decode(
              CreateSandboxRequest.self,
              from: request.body
            )
            let response = try await manager.create(templateID: createRequest.templateID)
            let body = try JSONEncoder().encode(response)
            send(status: 201, body: body, to: client)

          case ("DELETE", let path) where path.hasPrefix("/sandboxes/"):
            let sandboxID = String(path.dropFirst("/sandboxes/".count))
            if try await manager.delete(sandboxID: sandboxID) {
              send(status: 204, body: Data(), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          default:
            sendError(status: 404, message: "route not found", to: client)
          }
        } catch {
          sendError(
            status: Self.statusCode(for: error),
            message: error.localizedDescription,
            to: client
          )
        }
      }
    } catch {
      sendError(status: 400, message: error.localizedDescription, to: client)
    }
  }

  private func readRequest(from descriptor: Int32) throws -> HTTPRequest {
    var data = Data()
    var expectedSize: Int?

    while data.count < Self.maximumRequestBytes {
      var buffer = [UInt8](repeating: 0, count: 4096)
      var count: Int
      repeat {
        count = buffer.withUnsafeMutableBytes {
          Darwin.read(descriptor, $0.baseAddress, $0.count)
        }
      } while count < 0 && errno == EINTR
      guard count > 0 else { throw CubeVZError.runtime("HTTP client disconnected") }
      data.append(contentsOf: buffer.prefix(count))
      guard data.count <= Self.maximumRequestBytes else {
        throw CubeVZError.invalidArguments("HTTP request is too large")
      }

      if expectedSize == nil,
        let headerRange = data.range(of: Data("\r\n\r\n".utf8))
      {
        let header = String(decoding: data[..<headerRange.lowerBound], as: UTF8.self)
        let contentLength = try HTTPRequestParser.contentLength(from: header)
        guard contentLength <= Self.maximumRequestBytes - headerRange.upperBound else {
          throw CubeVZError.invalidArguments("HTTP request is too large")
        }
        expectedSize = headerRange.upperBound + contentLength
      }
      if let expectedSize, data.count >= expectedSize { break }
    }

    guard let headerRange = data.range(of: Data("\r\n\r\n".utf8)) else {
      throw CubeVZError.invalidArguments("invalid HTTP headers")
    }
    guard let expectedSize, data.count >= expectedSize else {
      throw CubeVZError.invalidArguments("incomplete HTTP request body")
    }
    let header = String(decoding: data[..<headerRange.lowerBound], as: UTF8.self)
    guard let requestLine = header.split(separator: "\r\n").first else {
      throw CubeVZError.invalidArguments("missing HTTP request line")
    }
    let components = requestLine.split(separator: " ")
    guard components.count >= 2 else {
      throw CubeVZError.invalidArguments("invalid HTTP request line")
    }
    return HTTPRequest(
      method: String(components[0]),
      path: String(components[1]),
      body: data.subdata(in: headerRange.upperBound..<expectedSize)
    )
  }

  private func send(status: Int, body: Data, to descriptor: Int32) {
    let reason: String
    switch status {
    case 200: reason = "OK"
    case 201: reason = "Created"
    case 204: reason = "No Content"
    case 400: reason = "Bad Request"
    case 404: reason = "Not Found"
    default: reason = "Internal Server Error"
    }
    let header = Data(
      "HTTP/1.1 \(status) \(reason)\r\nContent-Type: application/json\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
        .utf8
    )
    writeAll(header + body, to: descriptor)
    Darwin.close(descriptor)
  }

  private func sendError(status: Int, message: String, to descriptor: Int32) {
    let body = (try? JSONSerialization.data(withJSONObject: ["error": message])) ?? Data()
    send(status: status, body: body, to: descriptor)
  }

  private static func statusCode(for error: Error) -> Int {
    if error is DecodingError { return 400 }
    if let cubeError = error as? CubeVZError,
      case .invalidArguments = cubeError
    {
      return 400
    }
    return 500
  }

  private func writeAll(_ data: Data, to descriptor: Int32) {
    data.withUnsafeBytes { bytes in
      var offset = 0
      while offset < bytes.count {
        let written = Darwin.write(
          descriptor,
          bytes.baseAddress?.advanced(by: offset),
          bytes.count - offset
        )
        if written < 0 && errno == EINTR { continue }
        if written <= 0 { return }
        offset += written
      }
    }
  }

  private func posixError(_ operation: String) -> CubeVZError {
    .runtime("\(operation) failed: \(String(cString: strerror(errno)))")
  }
}
