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
    guard let separator = prefix.firstIndex(of: "-") else { return nil }
    let port = prefix[..<separator]
    guard port.allSatisfy(\.isNumber), port.count > 0 else { return nil }
    let sandboxID = String(prefix[prefix.index(after: separator)...])
    return sandboxID.isEmpty ? nil : sandboxID
  }

  var dataPlanePort: UInt32? {
    guard let host = headers["host"]?.lowercased() else { return nil }
    let prefix = host.split(separator: ".", maxSplits: 1).first.map(String.init) ?? host
    guard let separator = prefix.firstIndex(of: "-") else { return nil }
    let rawPort = prefix[..<separator]
    guard let port = UInt32(rawPort), port > 0, port <= UInt32(UInt16.max) else {
      return nil
    }
    return port
  }

  var trafficAccessToken: String? {
    headers["e2b-traffic-access-token"] ?? headers["cube-traffic-access-token"]
  }

  func guestRequest() -> Data {
    var request = "\(method) \(path) HTTP/1.1\r\n"
    for (name, value) in headers.sorted(by: { $0.key < $1.key }) {
      if name == "host" || name == "connection" || name == "proxy-connection" {
        continue
      }
      request += "\(name): \(value)\r\n"
    }
    request += "Host: 127.0.0.1:\(dataPlanePort ?? 49_983)\r\nConnection: close\r\n\r\n"
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

private struct RefreshRequest: Decodable {
  let duration: Int?
}

private struct SnapshotRequest: Decodable {
  let name: String?
}

private struct RollbackRequest: Decodable {
  let snapshotID: String
}

final class HTTPServer: @unchecked Sendable {
  private enum RequestError: Error, CustomStringConvertible, LocalizedError {
    case authorizationRequired
    case authorizationRejected
    case authorizationUnavailable(String)

    var status: Int {
      switch self {
      case .authorizationRequired, .authorizationRejected: return 401
      case .authorizationUnavailable: return 500
      }
    }

    var description: String {
      switch self {
      case .authorizationRequired:
        return "Missing authentication: provide Authorization: Bearer <token> or X-API-Key: <key>"
      case .authorizationRejected:
        return "Authentication rejected"
      case .authorizationUnavailable(let message):
        return message
      }
    }

    var errorDescription: String? { description }
  }

  private let bindAddress: String
  private let port: UInt16
  private let manager: SandboxManager
  private let scheduler: ClusterScheduler
  private let authCallbackURL: URL?
  private let apiKey: String?
  private let workerQueue = DispatchQueue(
    label: "com.tencent.cubesandbox.cube-vz-api",
    qos: .userInitiated,
    attributes: .concurrent
  )
  private var listenDescriptor: Int32 = -1

  init(
    bindAddress: String,
    port: UInt16,
    manager: SandboxManager,
    scheduler: ClusterScheduler,
    authCallbackURL: URL? = nil,
    apiKey: String? = nil
  ) {
    self.bindAddress = bindAddress
    self.port = port
    self.manager = manager
    self.scheduler = scheduler
    self.authCallbackURL = authCallbackURL
    self.apiKey = apiKey?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == true
      ? nil
      : apiKey?.trimmingCharacters(in: .whitespacesAndNewlines)
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
        Task { @MainActor [self, manager, scheduler] in
          do {
            if let remoteURL = scheduler.remoteURL(for: sandboxID) {
              workerQueue.async { [self] in
                proxyRemote(request: request, client: client, remoteURL: remoteURL)
              }
              return
            }
            let connection = try await manager.openDataPlane(
              sandboxID: sandboxID,
              trafficAccessToken: request.trafficAccessToken,
              guestPort: request.dataPlanePort ?? 49_983
            )
            workerQueue.async { [self] in
              proxy(request: request, client: client, connection: connection)
            }
          } catch {
            sendError(status: 502, message: Self.message(for: error), to: client)
          }
        }
        return
      }
      Task { @MainActor [self, manager, scheduler] in
        do {
          try await authorize(request)
          let routePath = Self.apiRoutePath(request.routePath)
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
          switch (request.method.uppercased(), routePath) {
          case ("GET", "/health"):
            send(status: 200, body: Data("{\"status\":\"ok\"}".utf8), to: client)

          case ("GET", "/sandboxes"), ("GET", "/v2/sandboxes"):
            let isV2 = routePath == "/v2/sandboxes"
            let response = filteredSandboxList(
              manager.list(),
              metadataQuery: request.queryItems["metadata"],
              state: isV2 ? request.queryItems["state"] : nil,
              limit: isV2 ? Int(request.queryItems["limit"] ?? "200") : nil
            )
            send(status: 200, body: try JSONEncoder().encode(response), to: client)

          case ("POST", "/sandboxes"):
            let createRequest = try JSONDecoder().decode(
              CreateSandboxRequest.self,
              from: request.body
            )
            let response = try await scheduler.create(
              request: createRequest,
              rawBody: request.body,
              headers: request.headers
            )
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

          case ("GET", let path) where Self.sandboxID(from: path, suffix: "logs") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "logs")!
            let limit = Int(request.queryItems["limit"] ?? "1000") ?? 1000
            let start = Int(request.queryItems["start"] ?? "0")
            if let response = try manager.logs(sandboxID: sandboxID, start: start, limit: limit) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("GET", let path) where Self.sandboxID(from: path, suffix: "logs", v2: true) != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "logs", v2: true)!
            let limit = Int(request.queryItems["limit"] ?? "1000") ?? 1000
            let cursor = Int(request.queryItems["cursor"] ?? "0")
            if let response = try manager.logsV2(sandboxID: sandboxID, cursor: cursor, limit: limit) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("POST", let path) where Self.sandboxID(from: path, suffix: "refreshes") != nil:
            let sandboxID = Self.sandboxID(from: path, suffix: "refreshes")!
            let body = try decode(RefreshRequest.self, from: request.body)
            if try manager.refresh(sandboxID: sandboxID, duration: body.duration ?? 0) {
              send(status: 204, body: Data(), to: client)
            } else {
              sendError(status: 404, message: "sandbox not found", to: client)
            }

          case ("GET", let path) where Self.sandboxID(from: path, suffix: "metrics") != nil:
            sendError(status: 501, message: "sandbox metrics are not available on CubeVZ", to: client)

          case ("PUT", let path) where Self.sandboxID(from: path, suffix: "network") != nil:
            sendError(status: 501, message: "network policy updates require sandbox restart on CubeVZ", to: client)

          case ("GET", "/snapshots"):
            let snapshots = manager.listSnapshots(
              originSandboxID: request.queryItems["sandboxID"]
            )
            let limit = min(max(Int(request.queryItems["limit"] ?? "100") ?? 100, 1), 100)
            let offset = max(Int(request.queryItems["nextToken"] ?? "0") ?? 0, 0)
            let page = Array(snapshots.dropFirst(offset).prefix(limit))
            let next = offset + page.count < snapshots.count ? String(offset + page.count) : nil
            send(
              status: 200,
              body: try JSONEncoder().encode(page),
              headers: next.map { ["x-next-token": $0] } ?? [:],
              to: client
            )

          case ("POST", "/templates"):
            // CubeVZ templates are provisioned out-of-band as a prepared VM
            // directory. Keep the CubeAPI build contract useful for SDKs by
            // acknowledging a local build request as an already-ready job.
            let payload = (try? JSONSerialization.jsonObject(with: request.body)) as? [String: Any]
            let requestedID = (payload?["templateID"] as? String)
              ?? (payload?["templateId"] as? String)
              ?? (payload?["name"] as? String)
            let templateID = requestedID?.isEmpty == false ? requestedID! : (manager.listTemplates().first?.templateID ?? "cube-vz")
            let response = TemplateBuildJobResponse(
              jobID: "build-\(UUID().uuidString.lowercased())",
              templateID: templateID,
              status: "completed",
              phase: "ready",
              progress: 100,
              errorMessage: ""
            )
            send(status: 202, body: try JSONEncoder().encode(response), to: client)

          case ("POST", let path) where Self.templateBuildStartPath(from: path) != nil:
            let (templateID, buildID) = Self.templateBuildStartPath(from: path)!
            if let response = manager.templateBuildStatus(templateID: templateID, buildID: buildID) {
              let job = TemplateBuildJobResponse(
                jobID: buildID,
                templateID: templateID,
                status: response.status,
                phase: response.status == "completed" ? "ready" : response.status,
                progress: response.progress,
                errorMessage: response.message
              )
              send(status: 202, body: try JSONEncoder().encode(job), to: client)
            } else {
              sendError(status: 404, message: "template or build not found", to: client)
            }
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

          case ("GET", "/templates"):
            send(status: 200, body: try JSONEncoder().encode(manager.listTemplates()), to: client)

          case ("GET", let path) where Self.templateID(from: path, suffix: nil) != nil:
            let templateID = Self.templateID(from: path, suffix: nil)!
            if let response = manager.getTemplate(templateID: templateID) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "template not found", to: client)
            }

          case ("PATCH", let path) where Self.templateID(from: path, suffix: nil) != nil:
            sendError(status: 501, message: "template updates are not supported by CubeVZ", to: client)

          case ("POST", let path) where Self.templateID(from: path, suffix: nil) != nil:
            let templateID = Self.templateID(from: path, suffix: nil)!
            let buildID = "build-\(UUID().uuidString.lowercased())"
            if let response = try manager.rebuildTemplate(
              templateID: templateID,
              buildID: buildID
            ) {
              send(status: 202, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "template not found", to: client)
            }

          case ("GET", let path) where Self.templateBuildPath(from: path) != nil:
            let (templateID, buildID) = Self.templateBuildPath(from: path)!
            if let response = manager.templateBuildStatus(
              templateID: templateID,
              buildID: buildID
            ) {
              send(status: 200, body: try JSONEncoder().encode(response), to: client)
            } else {
              sendError(status: 404, message: "template or build not found", to: client)
            }

          case ("GET", let path) where Self.templateBuildLogsPath(from: path) != nil:
            let (templateID, _) = Self.templateBuildLogsPath(from: path)!
            guard manager.getTemplate(templateID: templateID) != nil else {
              sendError(status: 404, message: "template not found", to: client)
              return
            }
            send(status: 200, body: Data("{\"logs\":[]}".utf8), to: client)

          case ("GET", "/templates/compat"):
            let templateID = manager.listTemplates().first?.templateID ?? "cube-vz"
            send(status: 200, body: Self.templateCompatibilityJSON(templateID: templateID), to: client)

          case ("POST", let path) where Self.templateCompatAdoptPath(from: path) != nil:
            let templateID = Self.templateCompatAdoptPath(from: path)!
            guard manager.getTemplate(templateID: templateID) != nil else {
              sendError(status: 404, message: "template not found", to: client)
              return
            }
            let body = try JSONSerialization.data(withJSONObject: ["updated": 0])
            send(status: 200, body: body, to: client)

          case ("GET", "/cluster/versions"):
            let body = try JSONSerialization.data(withJSONObject: [
              "overall": "compatible",
              "components": [
                "cubeVZ": "2026.16",
                "virtualizationFramework": "native",
              ],
            ])
            send(status: 200, body: body, to: client)

          case ("GET", "/config"):
            let body = try JSONSerialization.data(withJSONObject: [
              "authEnabled": authCallbackURL != nil || apiKey != nil,
              "backend": "cube-vz",
              "nodeID": scheduler.nodeID,
            ])
            send(status: 200, body: body, to: client)

          case ("GET", "/store/meta"):
            let body = try JSONSerialization.data(withJSONObject: [
              "backend": "apfs",
              "copyOnWrite": true,
              "healthy": true,
            ])
            send(status: 200, body: body, to: client)

          case ("POST", "/store/refresh"):
            let body = try JSONSerialization.data(withJSONObject: ["status": "ok"])
            send(status: 200, body: body, to: client)

          case ("GET", "/auth/session"):
            let authRequired = authCallbackURL != nil || apiKey != nil
            let hasCredential = request.headers["authorization"] != nil
              || request.headers["x-api-key"] != nil
              || request.headers["cube-api-key"] != nil
            let body = try JSONSerialization.data(withJSONObject: [
              "authRequired": authRequired,
              "authenticated": !authRequired || hasCredential,
            ])
            send(status: 200, body: body, to: client)

          case ("POST", "/auth/logout"):
            send(status: 204, body: Data(), to: client)

          case ("POST", "/auth/login"):
            sendError(status: 501, message: "CubeVZ uses API-key or callback authentication", to: client)

          case ("POST", "/auth/change-password"):
            sendError(status: 501, message: "password authentication is not configured", to: client)

          default:
            sendError(status: 404, message: "route not found", to: client)
          }
        } catch {
          sendError(status: Self.status(for: error), message: Self.message(for: error), to: client)
        }
      }
    } catch {
      sendError(status: 400, message: error.localizedDescription, to: client)
    }
  }

  private static func apiRoutePath(_ path: String) -> String {
    if path == "/cubeapi/v1" { return "/" }
    let prefix = "/cubeapi/v1/"
    return path.hasPrefix(prefix) ? String(path.dropFirst(prefix.count - 1)) : path
  }

  private static func sandboxID(from path: String, suffix: String?, v2: Bool = false) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    if v2 {
      guard components.count >= 3, components[0] == "v2", components[1] == "sandboxes" else {
        return nil
      }
    } else {
      guard components.count >= 2, components[0] == "sandboxes" else { return nil }
    }
    if let suffix {
      let suffixIndex = v2 ? 3 : 2
      guard components.count == suffixIndex + 1, components[suffixIndex] == Substring(suffix) else {
        return nil
      }
    } else if components.count != (v2 ? 3 : 2) {
      return nil
    }
    let sandboxID = String(components[v2 ? 2 : 1])
    return sandboxID.isEmpty ? nil : sandboxID
  }

  private static func templateID(from path: String, suffix: String?) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count == (suffix == nil ? 2 : 3), components.first == "templates" else {
      return nil
    }
    guard components[1] != "compat" else { return nil }
    if let suffix, components[2] != Substring(suffix) { return nil }
    return String(components[1])
  }

  private static func templateBuildPath(from path: String) -> (String, String)? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count == 5, components[0] == "templates", components[2] == "builds",
      components[4] == "status", components[1] != "compat"
    else { return nil }
    return (String(components[1]), String(components[3]))
  }

  private static func templateBuildStartPath(from path: String) -> (String, String)? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count == 4, components[0] == "templates", components[2] == "builds",
      components[1] != "compat"
    else { return nil }
    return (String(components[1]), String(components[3]))
  }

  private static func templateBuildLogsPath(from path: String) -> (String, String)? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count == 5, components[0] == "templates", components[2] == "builds",
      components[4] == "logs", components[1] != "compat"
    else { return nil }
    return (String(components[1]), String(components[3]))
  }

  private static func templateCompatAdoptPath(from path: String) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    guard components.count == 4, components[0] == "templates", components[1] == "compat",
      components[3] == "adopt-baseline"
    else { return nil }
    return String(components[2])
  }

  private static func anySandboxID(from path: String) -> String? {
    let components = path.split(separator: "/", omittingEmptySubsequences: true)
    if components.count >= 2, components[0] == "sandboxes" {
      return String(components[1])
    }
    if components.count >= 3, components[0] == "v2", components[1] == "sandboxes" {
      return String(components[2])
    }
    return nil
  }

  private func decode<T: Decodable>(_ type: T.Type, from body: Data) throws -> T {
    try JSONDecoder().decode(type, from: body.isEmpty ? Data("{}".utf8) : body)
  }

  private func authorize(_ request: HTTPRequest) async throws {
    let routePath = Self.apiRoutePath(request.routePath)
    // Health is intentionally public, matching CubeAPI's unauthenticated
    // health probe. Data-plane requests are handled before this method and
    // are authenticated by the per-sandbox traffic token instead.
    if routePath == "/health" || routePath.hasPrefix("/auth/") { return }
    guard apiKey != nil || authCallbackURL != nil else { return }

    let authorization = request.headers["authorization"]
    let bearer: String? = authorization.flatMap { value in
      let parts = value.split(separator: " ", maxSplits: 1)
      guard parts.count == 2, parts[0].lowercased() == "bearer" else { return nil }
      let token = String(parts[1]).trimmingCharacters(in: .whitespacesAndNewlines)
      return token.isEmpty ? nil : token
    }
    let apiKeyHeader = [
      request.headers["x-api-key"],
      request.headers["cube-api-key"],
    ].compactMap { $0?.trimmingCharacters(in: .whitespacesAndNewlines) }
      .first { !$0.isEmpty }

    guard bearer != nil || apiKeyHeader != nil else {
      throw RequestError.authorizationRequired
    }
    let presented = bearer ?? apiKeyHeader!
    if let expected = apiKey, presented == expected {
      return
    }
    guard let callback = authCallbackURL else {
      throw RequestError.authorizationRejected
    }

    var callbackRequest = URLRequest(url: callback)
    callbackRequest.httpMethod = "POST"
    callbackRequest.setValue(request.routePath, forHTTPHeaderField: "X-Request-Path")
    callbackRequest.setValue(request.method.uppercased(), forHTTPHeaderField: "X-Request-Method")
    if let bearer {
      callbackRequest.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization")
    } else if let apiKeyHeader {
      callbackRequest.setValue(apiKeyHeader, forHTTPHeaderField: "X-API-Key")
    }
    callbackRequest.timeoutInterval = 5
    do {
      let (_, response) = try await URLSession.shared.data(for: callbackRequest)
      guard let http = response as? HTTPURLResponse else {
        throw RequestError.authorizationUnavailable("auth callback returned a non-HTTP response")
      }
      guard http.statusCode == 200 else { throw RequestError.authorizationRejected }
    } catch let error as RequestError {
      throw error
    } catch {
      throw RequestError.authorizationUnavailable("auth callback unavailable: \(error.localizedDescription)")
    }
  }

  private func filteredSandboxList(
    _ list: [SandboxInfoResponse],
    metadataQuery: String?,
    state: String?,
    limit: Int?
  ) -> [SandboxInfoResponse] {
    var result = list
    if let state, !state.isEmpty, state == "running" || state == "paused" {
      result = result.filter { $0.state == state }
    }
    if let metadataQuery, !metadataQuery.isEmpty {
      let pairs = metadataQuery.split(separator: "&").compactMap { pair -> (String, String)? in
        let parts = pair.split(separator: "=", maxSplits: 1)
        guard parts.count == 2 else { return nil }
        return (
          String(parts[0]).removingPercentEncoding ?? String(parts[0]),
          String(parts[1]).removingPercentEncoding ?? String(parts[1])
        )
      }
      result = result.filter { info in
        guard let metadata = info.metadata else { return false }
        return pairs.allSatisfy { metadata[$0.0] == $0.1 }
      }
    }
    if let limit, limit > 0 { result = Array(result.prefix(min(limit, 1_000))) }
    return result
  }

  private static func templateCompatibilityJSON(templateID: String) -> Data {
    (try? JSONSerialization.data(withJSONObject: [
      "summary": [
        "staleTemplates": 0,
        "staleReplicas": 0,
        "affectedNodes": 0,
        "missingReplicas": 0,
        "unknownReplicas": 0,
      ],
      "templates": [[
        "templateID": templateID,
        "instanceType": "cube-vz",
        "overall": "compatible",
        "nodes": [[
          "nodeID": "cube-vz-local",
          "nodeIP": "127.0.0.1",
          "compatStatus": "compatible",
        ]],
      ]],
    ])) ?? Data("{\"summary\":{},\"templates\":[]}".utf8)
  }

  private static func status(for error: Error) -> Int {
    if let requestError = error as? RequestError { return requestError.status }
    if error is DecodingError { return 400 }
    guard let cubeError = error as? CubeVZError else { return 500 }
    switch cubeError {
    case .invalidArguments: return 400
    case .unsupported: return 501
    case .invalidManifest, .filesystem: return 500
    case .runtime(let message):
      let lower = message.lowercased()
      if lower.contains("not found") || lower.contains("unknown template") { return 404 }
      if lower.contains("not paused") || lower.contains("not running") || lower.contains("transition") {
        return 409
      }
      return 500
    }
  }

  private static func message(for error: Error) -> String {
    if let requestError = error as? RequestError { return requestError.description }
    if let cubeError = error as? CubeVZError { return cubeError.description }
    return error.localizedDescription
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
    guard writeAll(request.guestRequest(), to: guest) else {
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
    case 401: reason = "Unauthorized"
    case 403: reason = "Forbidden"
    case 404: reason = "Not Found"
    case 409: reason = "Conflict"
    case 413: reason = "Payload Too Large"
    case 422: reason = "Unprocessable Entity"
    case 429: reason = "Too Many Requests"
    case 501: reason = "Not Implemented"
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
    let body =
      (try? JSONSerialization.data(withJSONObject: ["code": status, "message": message]))
      ?? Data()
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
