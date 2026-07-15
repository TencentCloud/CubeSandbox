// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

final class HTTPServer: @unchecked Sendable {
  private static let maximumConnections = 64
  private static let maximumRequestBytes = 1_048_576
  private static let requestReadTimeoutNanoseconds: UInt64 = 5_000_000_000

  private let port: UInt16
  private let manager: SandboxManager
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-api",
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
      context: "control-plane"
    )

    workerQueue.async { [self] in
      acceptLoop()
    }
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
        sendError(status: 503, message: "control-plane capacity reached", to: client)
        continue
      }
      workerQueue.async { [self] in
        handle(client: client)
      }
    }
  }

  private func handle(client: Int32) {
    do {
      let request = try readRequest(from: client)
      Task { @MainActor [self, manager] in
        defer { connectionSlots.signal() }
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
          Self.log(error, context: "handle control request")
          let clientError = Self.clientError(for: error)
          sendError(
            status: clientError.status,
            message: clientError.message,
            to: client
          )
        }
      }
    } catch {
      Self.log(error, context: "parse control request")
      sendError(status: 400, message: "invalid HTTP request", to: client)
      connectionSlots.signal()
    }
  }

  private func readRequest(from descriptor: Int32) throws -> ParsedHTTPRequest {
    var data = Data()
    let deadline = DispatchTime.now().uptimeNanoseconds + Self.requestReadTimeoutNanoseconds

    while true {
      if let expectedSize = try HTTPRequestParser.expectedRequestSize(
        in: data,
        maximumBytes: Self.maximumRequestBytes
      ), data.count >= expectedSize {
        return try HTTPRequestParser.parse(data, maximumBytes: Self.maximumRequestBytes)
      }
      var buffer = [UInt8](repeating: 0, count: 4096)
      let count = try POSIXSocket.read(
        into: &buffer,
        from: descriptor,
        before: deadline,
        timeoutMessage: "HTTP request timed out",
        context: "control-plane"
      )
      guard count > 0 else { throw CubeVZError.runtime("HTTP client disconnected") }
      data.append(contentsOf: buffer.prefix(count))
    }
  }

  private func send(status: Int, body: Data, to descriptor: Int32) {
    let reason: String
    switch status {
    case 200: reason = "OK"
    case 201: reason = "Created"
    case 204: reason = "No Content"
    case 400: reason = "Bad Request"
    case 404: reason = "Not Found"
    case 503: reason = "Service Unavailable"
    default: reason = "Internal Server Error"
    }
    let header = Data(
      "HTTP/1.1 \(status) \(reason)\r\nContent-Type: application/json\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
        .utf8
    )
    _ = POSIXSocket.writeAll(header, to: descriptor)
    _ = POSIXSocket.writeAll(body, to: descriptor)
    Darwin.close(descriptor)
  }

  private func sendError(status: Int, message: String, to descriptor: Int32) {
    let body = (try? JSONSerialization.data(withJSONObject: ["error": message])) ?? Data()
    send(status: status, body: body, to: descriptor)
  }

  private static func clientError(for error: Error) -> (status: Int, message: String) {
    if error is DecodingError { return (400, "invalid request body") }
    if let cubeError = error as? CubeVZError,
      case .invalidArguments = cubeError
    {
      return (400, "invalid request")
    }
    return (500, "internal server error")
  }

  private static func log(_ error: Error, context: String) {
    FileHandle.standardError.write(
      Data("cube-vz-api: \(context): \(error.localizedDescription)\n".utf8)
    )
  }
}
