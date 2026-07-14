// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

struct ClusterRegistrationRequest: Codable {
  let nodeID: String
  let url: String
  let activeSandboxes: Int?
}

struct ClusterNodeResponse: Encodable {
  let nodeID: String
  let hostIP: String
  let instanceType = "cube-vz"
  let healthy: Bool
  let activeSandboxes: Int
  let heartbeatTime: String
  let localTemplates = ["cube-vz"]
}

struct ClusterOverviewResponse: Encodable {
  let nodeCount: Int
  let healthyNodes: Int
  let totalCpuMilli: Int
  let allocatableCpuMilli: Int
  let totalMemoryMB: Int
  let allocatableMemoryMB: Int
  let maxMvmSlots: Int
}

struct ForwardResponse {
  let status: Int
  let body: Data
  let headers: [String: String]
}

@MainActor
final class ClusterScheduler {
  private struct NodeRecord {
    let nodeID: String
    let url: URL
    var activeSandboxes: Int
    var lastHeartbeat: Date
  }

  private struct SandboxPlacement {
    let nodeID: String
  }

  private struct SandboxIDResponse: Decodable {
    let sandboxID: String
  }

  private let localNodeID: String
  private let advertiseURL: URL
  private let manager: SandboxManager
  private var nodes: [String: NodeRecord] = [:]
  private var placements: [String: SandboxPlacement] = [:]

  init(localNodeID: String, advertiseURL: URL, manager: SandboxManager) {
    self.localNodeID = localNodeID
    self.advertiseURL = advertiseURL
    self.manager = manager
  }

  var nodeID: String { localNodeID }

  func register(_ request: ClusterRegistrationRequest) throws {
    guard !request.nodeID.isEmpty, request.nodeID != localNodeID else {
      throw CubeVZError.invalidArguments("invalid remote nodeID")
    }
    guard let url = URL(string: request.url), url.scheme == "http", url.host != nil else {
      throw CubeVZError.invalidArguments("node url must be an http URL")
    }
    nodes[request.nodeID] = NodeRecord(
      nodeID: request.nodeID,
      url: url,
      activeSandboxes: max(
        0, request.activeSandboxes ?? nodes[request.nodeID]?.activeSandboxes ?? 0),
      lastHeartbeat: Date()
    )
  }

  func create(
    request: CreateSandboxRequest,
    rawBody: Data,
    headers: [String: String] = [:]
  ) async throws -> ForwardResponse {
    let nodeID = try selectNode(distributionScope: request.distributionScope)
    if nodeID == localNodeID {
      let response = try await manager.create(request: request)
      placements[response.sandboxID] = SandboxPlacement(nodeID: localNodeID)
      return ForwardResponse(
        status: 201,
        body: try JSONEncoder().encode(response),
        headers: [:]
      )
    }
    guard var node = nodes[nodeID] else {
      throw CubeVZError.runtime("selected node disappeared: \(nodeID)")
    }
    let response = try await forward(
      method: "POST",
      path: "/sandboxes",
      headers: headers.merging(["content-type": "application/json"]) { _, replacement in replacement },
      body: rawBody,
      to: node.url
    )
    if (200..<300).contains(response.status) {
      let decoded = try JSONDecoder().decode(SandboxIDResponse.self, from: response.body)
      placements[decoded.sandboxID] = SandboxPlacement(nodeID: nodeID)
      node.activeSandboxes += 1
      nodes[nodeID] = node
    }
    return response
  }

  func remoteURL(for sandboxID: String) -> URL? {
    guard let placement = placements[sandboxID], placement.nodeID != localNodeID else {
      return nil
    }
    return nodes[placement.nodeID]?.url
  }

  func forwardControl(
    method: String,
    path: String,
    headers: [String: String],
    body: Data,
    sandboxID: String
  ) async throws -> ForwardResponse? {
    guard let remoteURL = remoteURL(for: sandboxID) else { return nil }
    let response = try await forward(
      method: method,
      path: path,
      headers: headers,
      body: body,
      to: remoteURL
    )
    if method == "DELETE", (200..<300).contains(response.status),
      let placement = placements.removeValue(forKey: sandboxID),
      var node = nodes[placement.nodeID]
    {
      node.activeSandboxes = max(0, node.activeSandboxes - 1)
      nodes[placement.nodeID] = node
    }
    return response
  }

  func nodeResponses() -> [ClusterNodeResponse] {
    let now = Date()
    let local = ClusterNodeResponse(
      nodeID: localNodeID,
      hostIP: advertiseURL.host ?? "127.0.0.1",
      healthy: true,
      activeSandboxes: manager.list().count,
      heartbeatTime: Self.dateString(now)
    )
    let remote = nodes.values.map { node in
      ClusterNodeResponse(
        nodeID: node.nodeID,
        hostIP: node.url.host ?? "",
        healthy: now.timeIntervalSince(node.lastHeartbeat) < 15,
        activeSandboxes: node.activeSandboxes,
        heartbeatTime: Self.dateString(node.lastHeartbeat)
      )
    }
    return ([local] + remote).sorted { $0.nodeID < $1.nodeID }
  }

  func nodeResponse(nodeID: String) -> ClusterNodeResponse? {
    nodeResponses().first { $0.nodeID == nodeID }
  }

  func overview() -> ClusterOverviewResponse {
    let views = nodeResponses()
    let healthy = views.filter(\.healthy).count
    let totalCPU = healthy * 2_000
    let totalMemory = healthy * 2_048
    let active = views.reduce(0) { $0 + $1.activeSandboxes }
    return ClusterOverviewResponse(
      nodeCount: views.count,
      healthyNodes: healthy,
      totalCpuMilli: totalCPU,
      allocatableCpuMilli: max(0, totalCPU - active * 2_000),
      totalMemoryMB: totalMemory,
      allocatableMemoryMB: max(0, totalMemory - active * 2_048),
      maxMvmSlots: healthy
    )
  }

  func registrationPayload() -> ClusterRegistrationRequest {
    ClusterRegistrationRequest(
      nodeID: localNodeID,
      url: advertiseURL.absoluteString,
      activeSandboxes: manager.list().count
    )
  }

  func registerWithCoordinator(_ coordinatorURL: URL) async throws {
    let body = try JSONEncoder().encode(registrationPayload())
    let response = try await forward(
      method: "POST",
      path: "/cluster/nodes/register",
      headers: ["content-type": "application/json"],
      body: body,
      to: coordinatorURL
    )
    guard (200..<300).contains(response.status) else {
      throw CubeVZError.runtime("coordinator registration returned HTTP \(response.status)")
    }
  }

  private func selectNode(distributionScope: [String]?) throws -> String {
    let now = Date()
    var candidates: [(String, Int)] = [(localNodeID, manager.list().count)]
    candidates += nodes.values
      .filter { now.timeIntervalSince($0.lastHeartbeat) < 15 }
      .map { ($0.nodeID, $0.activeSandboxes) }
    if let scope = distributionScope, !scope.isEmpty {
      let allowed = Set(scope)
      candidates = candidates.filter { allowed.contains($0.0) }
    }
    guard
      let selected = candidates.min(by: { lhs, rhs in
        lhs.1 == rhs.1 ? lhs.0 < rhs.0 : lhs.1 < rhs.1
      })
    else {
      throw CubeVZError.runtime("no healthy node matches distributionScope")
    }
    return selected.0
  }

  private func forward(
    method: String,
    path: String,
    headers: [String: String],
    body: Data,
    to baseURL: URL
  ) async throws -> ForwardResponse {
    guard let url = URL(string: path, relativeTo: baseURL)?.absoluteURL else {
      throw CubeVZError.runtime("cannot build remote node URL")
    }
    var request = URLRequest(url: url)
    request.httpMethod = method
    request.httpBody = body.isEmpty ? nil : body
    for (name, value) in headers where name.lowercased() != "host" {
      request.setValue(value, forHTTPHeaderField: name)
    }
    request.timeoutInterval = 240
    let (data, response) = try await URLSession.shared.data(for: request)
    guard let http = response as? HTTPURLResponse else {
      throw CubeVZError.runtime("remote node returned a non-HTTP response")
    }
    var responseHeaders: [String: String] = [:]
    for (key, value) in http.allHeaderFields {
      responseHeaders[String(describing: key).lowercased()] = String(describing: value)
    }
    return ForwardResponse(status: http.statusCode, body: data, headers: responseHeaders)
  }

  nonisolated private static func dateString(_ date: Date) -> String {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter.string(from: date)
  }
}
