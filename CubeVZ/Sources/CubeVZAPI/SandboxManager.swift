// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
import Foundation

struct CreateSandboxRequest: Decodable {
  let templateID: String
  let timeout: Int?
  let lifecycle: SandboxLifecycleRequest?
  let secure: Bool?
  let allow_internet_access: Bool?
  let network: SandboxNetworkRequest?
  let metadata: [String: String]?
  let distributionScope: [String]?
  let envVars: [String: String]?

  private enum CodingKeys: String, CodingKey {
    case templateID, timeout, lifecycle, secure, allowInternetAccess, allow_internet_access
    case network, metadata, distributionScope, distribution_scope, envVars, envs
  }

  init(from decoder: Decoder) throws {
    let container = try decoder.container(keyedBy: CodingKeys.self)
    templateID = try container.decode(String.self, forKey: .templateID)
    timeout = try container.decodeIfPresent(Int.self, forKey: .timeout)
    lifecycle = try container.decodeIfPresent(SandboxLifecycleRequest.self, forKey: .lifecycle)
    secure = try container.decodeIfPresent(Bool.self, forKey: .secure)
    allow_internet_access = try container.decodeIfPresent(Bool.self, forKey: .allowInternetAccess)
      ?? container.decodeIfPresent(Bool.self, forKey: .allow_internet_access)
    network = try container.decodeIfPresent(SandboxNetworkRequest.self, forKey: .network)
    metadata = try container.decodeIfPresent([String: String].self, forKey: .metadata)
    distributionScope = try container.decodeIfPresent([String].self, forKey: .distributionScope)
      ?? container.decodeIfPresent([String].self, forKey: .distribution_scope)
    envVars = try container.decodeIfPresent([String: String].self, forKey: .envVars)
      ?? container.decodeIfPresent([String: String].self, forKey: .envs)
  }
}

struct SandboxLifecycleRequest: Decodable {
  let onTimeout: String?
  let autoResume: Bool?
}

struct SandboxNetworkRequest: Codable {
  let allowPublicTraffic: Bool?
  let allowOut: [String]?
  let denyOut: [String]?
  let maskRequestHost: String?
  let rules: [EgressRuleRequest]?
}

struct EgressRuleRequest: Codable {
  let name: String
  let match: EgressRuleMatchRequest
  let action: EgressRuleActionRequest
}

struct EgressRuleMatchRequest: Codable {
  let sni: String?
  let host: String?
  let method: [String]?
  let path: String?
  let scheme: String?
}

struct EgressRuleActionRequest: Codable {
  let allow: Bool
  let audit: String?
  let inject: [EgressRuleInjectRequest]?
}

struct EgressRuleInjectRequest: Codable {
  let header: String
  let secret: String
  let format: String?
}

struct SandboxResponse: Encodable {
  let templateID: String
  let sandboxID: String
  let clientID: String
  let envdVersion = "2026.16"
  let envdAccessToken: String?
  let trafficAccessToken: String?
  let domain: String? = "cube.local"
}

struct SandboxLog: Encodable {
  let timestamp: String
  let line: String
}

struct SandboxLogEntry: Encodable {
  let timestamp: String
  let message: String
  let level: String
  let fields: [String: String]
}

struct SandboxLogsResponse: Encodable {
  let logs: [SandboxLog]
  let logEntries: [SandboxLogEntry]
}

struct SandboxLogsV2Response: Encodable {
  let logs: [SandboxLogEntry]
}

struct TemplateInfoResponse: Encodable {
  let templateID: String
  let instanceType: String
  let version: String
  let status: String
  let lastError: String?
  let createdAt: String
  let imageInfo: String
  let jobID: String?
  let networkType: String
  let allowInternetAccess: Bool
}

struct TemplateBuildJobResponse: Encodable {
  let jobID: String
  let templateID: String
  let status: String
  let phase: String
  let progress: Int
  let errorMessage: String
}

struct TemplateBuildStatusResponse: Encodable {
  let buildID: String
  let templateID: String
  let status: String
  let progress: Int
  let message: String
}

private struct PersistedSandbox: Codable {
  let sandboxID: String
  let templateID: String
  let startedAt: Date
  let timeout: Int?
  let metadata: [String: String]?
  let envVars: [String: String]?
  let network: SandboxNetworkRequest?
  let allowInternetAccess: Bool
  let trafficAccessToken: String?
  let envdAccessToken: String?
  let autoResume: Bool
  let onTimeout: String
  let cpuCount: Int
  let memoryMiB: UInt64
  let diskSizeMiB: Int?
  let expiresAt: Date?
}

struct SandboxInfoResponse: Encodable {
  let templateID: String
  let sandboxID: String
  let clientID: String
  let startedAt: String
  let endAt: String?
  let cpuCount: Int
  let memoryMB: Int
  let diskSizeMB: Int?
  let metadata: [String: String]?
  let state: String
  let envdVersion = "2026.16"
  let envdAccessToken: String?
  let domain: String? = "cube.local"
}

struct SnapshotInfoResponse: Encodable {
  let snapshotID: String
  let names: [String]
}

struct SnapshotListItemResponse: Encodable {
  let snapshotID: String
  let names: [String]
  let status = "ready"
  let originSandboxID: String
  let createdAt: String
  let updatedAt: String
}

struct RollbackResponse: Encodable {
  let sandboxID: String
  let snapshotID: String
  let operationID: String
  let status = "success"
}

@MainActor
final class SandboxManager {
  private enum LifecycleState: String {
    case running
    case paused
    case pausing
  }

  private struct SandboxRecord {
    let sandboxID: String
    let templateID: String
    let directory: VMDirectory
    var virtualMachine: ManagedVM?
    var state: LifecycleState
    let startedAt: Date
    var timeout: Int?
    let metadata: [String: String]?
    let envVars: [String: String]?
    let network: SandboxNetworkRequest?
    let allowInternetAccess: Bool
    let trafficAccessToken: String?
    let envdAccessToken: String?
    let autoResume: Bool
    let onTimeout: String
    let cpuCount: Int
    let memoryMiB: UInt64
    let diskSizeMiB: Int?
    var expiresAt: Date?
  }

  private struct SnapshotRecord {
    let snapshotID: String
    let names: [String]
    let originSandboxID: String
    let directory: VMDirectory
    let createdAt: Date
  }

  private let templateID: String
  private let templateDirectory: VMDirectory
  private let sandboxesDirectory: URL
  private let snapshotsDirectory: URL
  private var sandboxes: [String: SandboxRecord] = [:]
  private var snapshots: [String: SnapshotRecord] = [:]
  private var expirationTasks: [String: Task<Void, Never>] = [:]
  private var sandboxLogs: [String: [SandboxLog]] = [:]

  init(templateID: String, templateDirectory: URL, sandboxesDirectory: URL) throws {
    self.templateID = templateID
    self.templateDirectory = VMDirectory(url: templateDirectory)
    self.sandboxesDirectory = sandboxesDirectory
    snapshotsDirectory = sandboxesDirectory.deletingLastPathComponent().appendingPathComponent(
      "snapshots",
      isDirectory: true
    )

    let manifest = try self.templateDirectory.loadManifest()
    try self.templateDirectory.validateFiles(for: manifest)
    guard FileManager.default.fileExists(atPath: self.templateDirectory.stateURL.path) else {
      throw CubeVZError.invalidManifest(
        "template has no saved state: \(self.templateDirectory.stateURL.path)"
      )
    }
    try FileManager.default.createDirectory(
      at: sandboxesDirectory,
      withIntermediateDirectories: true
    )
    try FileManager.default.createDirectory(
      at: snapshotsDirectory,
      withIntermediateDirectories: true
    )
    loadPersistedSandboxes()
  }

  func create(request: CreateSandboxRequest) async throws -> SandboxResponse {
    let source: VMDirectory
    if request.templateID == templateID {
      source = templateDirectory
    } else if let snapshot = snapshots[request.templateID] {
      source = snapshot.directory
    } else {
      throw CubeVZError.invalidArguments("unknown templateID: \(request.templateID)")
    }
    try validate(request: request)

    let sandboxID = "sb-\(UUID().uuidString.lowercased())"
    let destination = sandboxesDirectory.appendingPathComponent(sandboxID, isDirectory: true)
    let directory = try VMTemplateCloner.clone(template: source, to: destination)
    var virtualMachine: ManagedVM?

    do {
      let manifest = try directory.loadManifest()
      let createdVirtualMachine = try ManagedVM(directory: directory, manifest: manifest)
      virtualMachine = createdVirtualMachine
      _ = try await createdVirtualMachine.start()
      try await createdVirtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        createdVirtualMachine,
        envVars: request.envVars,
        network: request.network
      )
      try await applyNetworkPolicy(
        createdVirtualMachine,
        allowInternetAccess: request.allow_internet_access ?? true,
        network: request.network
      )

      let trafficAccessToken =
        request.network?.allowPublicTraffic == false ? UUID().uuidString.lowercased() : nil
      let envdAccessToken: String? = nil
      let timeout = request.timeout
      let expiresAt = Self.expirationDate(timeout: timeout)
      let record = SandboxRecord(
        sandboxID: sandboxID,
        templateID: request.templateID,
        directory: directory,
        virtualMachine: createdVirtualMachine,
        state: .running,
        startedAt: Date(),
        timeout: request.timeout,
        metadata: request.metadata,
        envVars: request.envVars,
        network: request.network,
        allowInternetAccess: request.allow_internet_access ?? true,
        trafficAccessToken: trafficAccessToken,
        envdAccessToken: envdAccessToken,
        autoResume: request.lifecycle?.autoResume ?? false,
        onTimeout: request.lifecycle?.onTimeout ?? "kill",
        cpuCount: manifest.cpuCount,
        memoryMiB: manifest.memoryMiB,
        diskSizeMiB: Self.diskSizeMiB(directory: directory, manifest: manifest),
        expiresAt: expiresAt
      )
      sandboxes[sandboxID] = record
      persist(record)
      sandboxLogs[sandboxID] = [Self.log(line: "sandbox started")]
      scheduleExpiration(for: sandboxID)
      return response(for: record, exposeTrafficToken: true)
    } catch {
      if let virtualMachine { try? await virtualMachine.shutdown() }
      try? FileManager.default.removeItem(at: destination)
      throw error
    }
  }

  func list() -> [SandboxInfoResponse] {
    sandboxes.values
      .sorted { $0.startedAt < $1.startedAt }
      .map(info(for:))
  }

  func get(sandboxID: String) -> SandboxInfoResponse? {
    sandboxes[sandboxID].map(info(for:))
  }

  func openDataPlane(
    sandboxID: String,
    trafficAccessToken: String?,
    guestPort: UInt32 = 49_983
  ) async throws -> VMStreamConnection {
    guard var sandbox = sandboxes[sandboxID] else {
      throw CubeVZError.runtime("sandbox not found: \(sandboxID)")
    }
    if let expectedToken = sandbox.trafficAccessToken, trafficAccessToken != expectedToken {
      throw CubeVZError.runtime("sandbox traffic access token is invalid")
    }
    if sandbox.state == .paused, sandbox.autoResume {
      _ = try await resume(sandboxID: sandboxID, timeout: nil)
      guard let resumed = sandboxes[sandboxID] else {
        throw CubeVZError.runtime("sandbox disappeared during resume: \(sandboxID)")
      }
      sandbox = resumed
    }
    guard sandbox.state == .running, let virtualMachine = sandbox.virtualMachine else {
      throw CubeVZError.runtime("sandbox is not running: \(sandboxID)")
    }
    if guestPort != 49_983 && guestPort != 49_999 {
      let response = try await virtualMachine.executeControlCommand("FORWARD \(guestPort)")
      guard response == "OK\n" else {
        throw CubeVZError.runtime("guest cannot forward port \(guestPort)")
      }
    }
    var lastError: Error?
    for _ in 0..<20 {
      do {
        return try await virtualMachine.connect(toGuestPort: guestPort)
      } catch {
        lastError = error
        try? await Task.sleep(for: .milliseconds(25))
      }
    }
    throw lastError ?? CubeVZError.runtime("cannot connect to guest port \(guestPort)")
  }

  func logs(sandboxID: String, start: Int?, limit: Int) throws -> SandboxLogsResponse? {
    guard sandboxes[sandboxID] != nil else { return nil }
    let entries = sandboxLogs[sandboxID] ?? []
    let offset = max(0, start ?? 0)
    let boundedLimit = min(max(0, limit), 10_000)
    let selected = Array(entries.dropFirst(offset).prefix(boundedLimit == 0 ? entries.count : boundedLimit))
    let logEntries = selected.map { entry in
      SandboxLogEntry(
        timestamp: entry.timestamp,
        message: entry.line,
        level: "info",
        fields: [:]
      )
    }
    return SandboxLogsResponse(logs: selected, logEntries: logEntries)
  }

  func logsV2(sandboxID: String, cursor: Int?, limit: Int) throws -> SandboxLogsV2Response? {
    guard let response = try logs(sandboxID: sandboxID, start: cursor, limit: limit) else {
      return nil
    }
    return SandboxLogsV2Response(logs: response.logEntries)
  }

  func refresh(sandboxID: String, duration: Int) throws -> Bool {
    guard duration >= 0 && duration <= 3_600 else {
      throw CubeVZError.invalidArguments("refresh duration must be between 0 and 3600 seconds")
    }
    guard var sandbox = sandboxes[sandboxID] else { return false }
    sandbox.timeout = duration
    sandbox.expiresAt = Date().addingTimeInterval(TimeInterval(duration))
    sandboxes[sandboxID] = sandbox
    persist(sandbox)
    appendLog(sandboxID, line: "sandbox refreshed for \(duration)s")
    scheduleExpiration(for: sandboxID)
    return true
  }

  func listTemplates() -> [TemplateInfoResponse] {
    let base = templateInfo(templateID: templateID, createdAt: nil)
    let snapshotTemplates = snapshots.values.sorted { $0.createdAt < $1.createdAt }.map {
      templateInfo(templateID: $0.snapshotID, createdAt: $0.createdAt)
    }
    return [base] + snapshotTemplates
  }

  func getTemplate(templateID requestedID: String) -> TemplateInfoResponse? {
    if requestedID == templateID { return templateInfo(templateID: requestedID, createdAt: nil) }
    guard let snapshot = snapshots[requestedID] else { return nil }
    return templateInfo(templateID: requestedID, createdAt: snapshot.createdAt)
  }

  func rebuildTemplate(templateID requestedID: String, buildID: String) throws -> TemplateBuildJobResponse? {
    guard getTemplate(templateID: requestedID) != nil else { return nil }
    return TemplateBuildJobResponse(
      jobID: buildID,
      templateID: requestedID,
      status: "completed",
      phase: "ready",
      progress: 100,
      errorMessage: ""
    )
  }

  func templateBuildStatus(templateID requestedID: String, buildID: String) -> TemplateBuildStatusResponse? {
    guard getTemplate(templateID: requestedID) != nil else { return nil }
    return TemplateBuildStatusResponse(
      buildID: buildID,
      templateID: requestedID,
      status: "completed",
      progress: 100,
      message: "template is ready"
    )
  }

  func pause(sandboxID: String) async throws -> Bool {
    guard var sandbox = sandboxes[sandboxID] else { return false }
    guard sandbox.state == .running, let virtualMachine = sandbox.virtualMachine else {
      throw CubeVZError.runtime("sandbox is not running: \(sandboxID)")
    }
    sandbox.state = .pausing
    sandboxes[sandboxID] = sandbox
    do {
      try await virtualMachine.saveStateAndStop()
      sandbox.virtualMachine = nil
      sandbox.state = .paused
      sandboxes[sandboxID] = sandbox
      persist(sandbox)
      appendLog(sandboxID, line: "sandbox paused")
      return true
    } catch {
      sandbox.state = .running
      sandboxes[sandboxID] = sandbox
      throw error
    }
  }

  func resume(sandboxID: String, timeout: Int?) async throws -> SandboxResponse? {
    guard var sandbox = sandboxes[sandboxID] else { return nil }
    guard sandbox.state == .paused else {
      throw CubeVZError.runtime("sandbox is not paused: \(sandboxID)")
    }
    let manifest = try sandbox.directory.loadManifest()
    let virtualMachine = try ManagedVM(directory: sandbox.directory, manifest: manifest)
    do {
      _ = try await virtualMachine.start()
      try await virtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        virtualMachine,
        envVars: sandbox.envVars,
        network: sandbox.network
      )
      try await applyNetworkPolicy(
        virtualMachine,
        allowInternetAccess: sandbox.allowInternetAccess,
        network: sandbox.network
      )
      sandbox.virtualMachine = virtualMachine
      sandbox.state = .running
      if let timeout { sandbox.timeout = timeout }
      if let timeout { sandbox.expiresAt = Self.expirationDate(timeout: timeout) }
      sandboxes[sandboxID] = sandbox
      persist(sandbox)
      appendLog(sandboxID, line: "sandbox resumed")
      scheduleExpiration(for: sandboxID)
      return response(for: sandbox, exposeTrafficToken: false)
    } catch {
      try? await virtualMachine.shutdown()
      throw error
    }
  }

  func connect(sandboxID: String, timeout: Int?) async throws -> SandboxResponse? {
    guard let sandbox = sandboxes[sandboxID] else { return nil }
    if sandbox.state == .paused {
      return try await resume(sandboxID: sandboxID, timeout: timeout)
    }
    guard sandbox.state == .running else {
      throw CubeVZError.runtime("sandbox is transitioning: \(sandboxID)")
    }
    var updated = sandbox
    if let timeout {
      updated.timeout = timeout
      updated.expiresAt = Self.expirationDate(timeout: timeout)
      sandboxes[sandboxID] = updated
      persist(updated)
      scheduleExpiration(for: sandboxID)
    }
    appendLog(sandboxID, line: "sandbox connected")
    return response(for: updated, exposeTrafficToken: false)
  }

  func setTimeout(sandboxID: String, timeout: Int) throws -> Bool {
    guard timeout >= 0 || timeout == -1 else {
      throw CubeVZError.invalidArguments("timeout must be non-negative or -1")
    }
    guard var sandbox = sandboxes[sandboxID] else { return false }
    sandbox.timeout = timeout
    sandbox.expiresAt = Self.expirationDate(timeout: timeout)
    sandboxes[sandboxID] = sandbox
    persist(sandbox)
    appendLog(sandboxID, line: "sandbox timeout set to \(timeout)s")
    scheduleExpiration(for: sandboxID)
    return true
  }

  func createSnapshot(sandboxID: String, name: String?) async throws -> SnapshotInfoResponse? {
    guard var sandbox = sandboxes[sandboxID] else { return nil }
    let wasRunning = sandbox.state == .running
    if wasRunning {
      _ = try await pause(sandboxID: sandboxID)
      guard let paused = sandboxes[sandboxID] else {
        throw CubeVZError.runtime("sandbox disappeared while snapshotting: \(sandboxID)")
      }
      sandbox = paused
    } else if sandbox.state != .paused {
      throw CubeVZError.runtime("sandbox is transitioning: \(sandboxID)")
    }

    let snapshotID = "snap-\(UUID().uuidString.lowercased())"
    let names = name.map { [$0] } ?? []
    let destination = snapshotsDirectory.appendingPathComponent(snapshotID, isDirectory: true)
    do {
      let snapshotDirectory = try VMTemplateCloner.clone(
        template: sandbox.directory,
        to: destination
      )
      let record = SnapshotRecord(
        snapshotID: snapshotID,
        names: names,
        originSandboxID: sandboxID,
        directory: snapshotDirectory,
        createdAt: Date()
      )
      snapshots[snapshotID] = record
      appendLog(sandboxID, line: "snapshot created: \(snapshotID)")
      if wasRunning { _ = try await resume(sandboxID: sandboxID, timeout: nil) }
      return SnapshotInfoResponse(snapshotID: snapshotID, names: names)
    } catch {
      if wasRunning { _ = try? await resume(sandboxID: sandboxID, timeout: nil) }
      try? FileManager.default.removeItem(at: destination)
      throw error
    }
  }

  func listSnapshots(originSandboxID: String?) -> [SnapshotListItemResponse] {
    snapshots.values
      .filter { originSandboxID == nil || $0.originSandboxID == originSandboxID }
      .sorted { $0.createdAt < $1.createdAt }
      .map { snapshot in
        SnapshotListItemResponse(
          snapshotID: snapshot.snapshotID,
          names: snapshot.names,
          originSandboxID: snapshot.originSandboxID,
          createdAt: Self.dateString(snapshot.createdAt),
          updatedAt: Self.dateString(snapshot.createdAt)
        )
      }
  }

  func deleteSnapshot(snapshotID: String) throws -> Bool {
    guard let snapshot = snapshots[snapshotID] else { return false }
    try FileManager.default.removeItem(at: snapshot.directory.url)
    snapshots.removeValue(forKey: snapshotID)
    return true
  }

  func rollback(sandboxID: String, snapshotID: String) async throws -> RollbackResponse? {
    guard var sandbox = sandboxes[sandboxID] else { return nil }
    guard let snapshot = snapshots[snapshotID] else {
      throw CubeVZError.invalidArguments("snapshot not found: \(snapshotID)")
    }
    if let virtualMachine = sandbox.virtualMachine { try await virtualMachine.shutdown() }

    let replacementURL = sandboxesDirectory.appendingPathComponent(
      ".\(sandboxID).rollback-\(UUID().uuidString)",
      isDirectory: true
    )
    let backupURL = sandboxesDirectory.appendingPathComponent(
      ".\(sandboxID).backup-\(UUID().uuidString)",
      isDirectory: true
    )
    _ = try VMTemplateCloner.clone(template: snapshot.directory, to: replacementURL)
    do {
      try FileManager.default.moveItem(at: sandbox.directory.url, to: backupURL)
      try FileManager.default.moveItem(at: replacementURL, to: sandbox.directory.url)
      let manifest = try sandbox.directory.loadManifest()
      let virtualMachine = try ManagedVM(directory: sandbox.directory, manifest: manifest)
      _ = try await virtualMachine.start()
      try await virtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        virtualMachine,
        envVars: sandbox.envVars,
        network: sandbox.network
      )
      try await applyNetworkPolicy(
        virtualMachine,
        allowInternetAccess: sandbox.allowInternetAccess,
        network: sandbox.network
      )
      sandbox.virtualMachine = virtualMachine
      sandbox.state = .running
      sandboxes[sandboxID] = sandbox
      persist(sandbox)
      appendLog(sandboxID, line: "sandbox rolled back to \(snapshotID)")
      scheduleExpiration(for: sandboxID)
      try? FileManager.default.removeItem(at: backupURL)
      return RollbackResponse(
        sandboxID: sandboxID,
        snapshotID: snapshotID,
        operationID: UUID().uuidString.lowercased()
      )
    } catch {
      try? FileManager.default.removeItem(at: sandbox.directory.url)
      if FileManager.default.fileExists(atPath: backupURL.path) {
        try? FileManager.default.moveItem(at: backupURL, to: sandbox.directory.url)
      }
      try? FileManager.default.removeItem(at: replacementURL)
      throw error
    }
  }

  func delete(sandboxID: String) async throws -> Bool {
    guard let sandbox = sandboxes[sandboxID] else { return false }
    if let virtualMachine = sandbox.virtualMachine { try await virtualMachine.shutdown() }
    try FileManager.default.removeItem(at: sandbox.directory.url)
    expirationTasks[sandboxID]?.cancel()
    expirationTasks.removeValue(forKey: sandboxID)
    sandboxes.removeValue(forKey: sandboxID)
    sandboxLogs.removeValue(forKey: sandboxID)
    return true
  }

  private func response(for sandbox: SandboxRecord, exposeTrafficToken: Bool) -> SandboxResponse {
    SandboxResponse(
      templateID: sandbox.templateID,
      sandboxID: sandbox.sandboxID,
      clientID: "cube-vz-local",
      envdAccessToken: sandbox.envdAccessToken,
      trafficAccessToken: exposeTrafficToken ? sandbox.trafficAccessToken : nil
    )
  }

  private func info(for sandbox: SandboxRecord) -> SandboxInfoResponse {
    let endAt = sandbox.expiresAt.map(Self.dateString)
    return SandboxInfoResponse(
      templateID: sandbox.templateID,
      sandboxID: sandbox.sandboxID,
      clientID: "cube-vz-local",
      startedAt: Self.dateString(sandbox.startedAt),
      endAt: endAt,
      cpuCount: sandbox.cpuCount,
      memoryMB: Int(sandbox.memoryMiB),
      diskSizeMB: sandbox.diskSizeMiB,
      metadata: sandbox.metadata,
      state: sandbox.state.rawValue,
      envdAccessToken: sandbox.envdAccessToken
    )
  }

  private func validate(request: CreateSandboxRequest) throws {
    if let timeout = request.timeout, timeout < -1 {
      throw CubeVZError.invalidArguments("timeout must be non-negative or -1")
    }
    if let onTimeout = request.lifecycle?.onTimeout, onTimeout != "kill" && onTimeout != "pause" {
      throw CubeVZError.invalidArguments("lifecycle.onTimeout must be kill or pause")
    }
    for (key, _) in request.envVars ?? [:] {
      guard !key.isEmpty, !key.contains("="), !key.unicodeScalars.contains(Unicode.Scalar(0)) else {
        throw CubeVZError.invalidArguments("invalid environment variable name: \(key)")
      }
    }
    _ = try networkPolicyTargets(network: request.network)
  }

  private func scheduleExpiration(for sandboxID: String) {
    expirationTasks[sandboxID]?.cancel()
    expirationTasks.removeValue(forKey: sandboxID)
    guard let expiresAt = sandboxes[sandboxID]?.expiresAt else { return }
    let delay = max(0, expiresAt.timeIntervalSinceNow)
    expirationTasks[sandboxID] = Task { [weak self] in
      if delay > 0 {
        try? await Task.sleep(for: .seconds(delay))
      }
      guard !Task.isCancelled else { return }
      await self?.expire(sandboxID: sandboxID)
    }
  }

  private func loadPersistedSandboxes() {
    let manager = FileManager.default
    guard let entries = try? manager.contentsOfDirectory(
      at: sandboxesDirectory,
      includingPropertiesForKeys: [.isDirectoryKey],
      options: [.skipsHiddenFiles]
    ) else { return }
    let decoder = JSONDecoder()
    for directoryURL in entries {
      let metadataURL = directoryURL.appendingPathComponent("sandbox.json")
      guard let data = try? Data(contentsOf: metadataURL),
        let persisted = try? decoder.decode(PersistedSandbox.self, from: data)
      else { continue }
      let directory = VMDirectory(url: directoryURL)
      guard let manifest = try? directory.loadManifest(),
        (try? directory.validateFiles(for: manifest)) != nil
      else { continue }
      let record = SandboxRecord(
        sandboxID: persisted.sandboxID,
        templateID: persisted.templateID,
        directory: directory,
        virtualMachine: nil,
        state: .paused,
        startedAt: persisted.startedAt,
        timeout: persisted.timeout,
        metadata: persisted.metadata,
        envVars: persisted.envVars,
        network: persisted.network,
        allowInternetAccess: persisted.allowInternetAccess,
        trafficAccessToken: persisted.trafficAccessToken,
        envdAccessToken: persisted.envdAccessToken,
        autoResume: persisted.autoResume,
        onTimeout: persisted.onTimeout,
        cpuCount: persisted.cpuCount,
        memoryMiB: persisted.memoryMiB,
        diskSizeMiB: persisted.diskSizeMiB,
        expiresAt: persisted.expiresAt
      )
      sandboxes[persisted.sandboxID] = record
      sandboxLogs[persisted.sandboxID] = [Self.log(line: "sandbox recovered")]
      scheduleExpiration(for: persisted.sandboxID)
    }
  }

  private func persist(_ sandbox: SandboxRecord) {
    let persisted = PersistedSandbox(
      sandboxID: sandbox.sandboxID,
      templateID: sandbox.templateID,
      startedAt: sandbox.startedAt,
      timeout: sandbox.timeout,
      metadata: sandbox.metadata,
      envVars: sandbox.envVars,
      network: sandbox.network,
      allowInternetAccess: sandbox.allowInternetAccess,
      trafficAccessToken: sandbox.trafficAccessToken,
      envdAccessToken: sandbox.envdAccessToken,
      autoResume: sandbox.autoResume,
      onTimeout: sandbox.onTimeout,
      cpuCount: sandbox.cpuCount,
      memoryMiB: sandbox.memoryMiB,
      diskSizeMiB: sandbox.diskSizeMiB,
      expiresAt: sandbox.expiresAt
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    try? encoder.encode(persisted).write(
      to: sandbox.directory.url.appendingPathComponent("sandbox.json"),
      options: [.atomic]
    )
  }

  private func expire(sandboxID: String) async {
    guard let sandbox = sandboxes[sandboxID], sandbox.expiresAt != nil else { return }
    expirationTasks.removeValue(forKey: sandboxID)
    appendLog(sandboxID, line: "sandbox timeout expired")
    do {
      if sandbox.onTimeout == "pause" {
        _ = try await pause(sandboxID: sandboxID)
      } else {
        _ = try await delete(sandboxID: sandboxID)
      }
    } catch {
      appendLog(sandboxID, line: "sandbox timeout action failed: \(error.localizedDescription)")
    }
  }

  private func appendLog(_ sandboxID: String, line: String) {
    sandboxLogs[sandboxID, default: []].append(Self.log(line: line))
    if let entries = sandboxLogs[sandboxID], entries.count > 10_000 {
      sandboxLogs[sandboxID] = Array(entries.suffix(10_000))
    }
  }

  private func templateInfo(templateID requestedID: String, createdAt: Date?) -> TemplateInfoResponse {
    TemplateInfoResponse(
      templateID: requestedID,
      instanceType: "cube-vz",
      version: "native",
      status: "ready",
      lastError: nil,
      createdAt: Self.dateString(createdAt ?? Date()),
      imageInfo: "ARM64 Linux guest on Apple Virtualization.framework",
      jobID: nil,
      networkType: "nat",
      allowInternetAccess: true
    )
  }

  private func initializeEnvd(
    _ virtualMachine: ManagedVM,
    envVars: [String: String]?,
    network: SandboxNetworkRequest?
  ) async throws {
    var effectiveEnvVars = envVars ?? [:]
    let proxyURL = (network?.rules?.isEmpty == false) ? "http://127.0.0.1:18080" : ""
    for key in ["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"]
    {
      effectiveEnvVars[key] = proxyURL
    }
    effectiveEnvVars["NO_PROXY"] = "127.0.0.1,localhost"
    effectiveEnvVars["no_proxy"] = "127.0.0.1,localhost"
    effectiveEnvVars["SSL_CERT_FILE"] = "/etc/ssl/certs/ca-certificates.crt"
    let body = try JSONSerialization.data(withJSONObject: [
      "envVars": effectiveEnvVars,
      "defaultUser": "root",
      "defaultWorkdir": "/root",
      "timestamp": ISO8601DateFormatter().string(from: Date()),
    ])
    let response = try await guestHTTPRequest(
      virtualMachine,
      method: "POST",
      path: "/init",
      body: body,
      contentType: "application/json"
    )
    guard response.hasPrefix("HTTP/1.1 204") else {
      let firstLine = response.split(separator: "\r\n", maxSplits: 1).first.map(String.init)
      throw CubeVZError.runtime("envd initialization failed: \(firstLine ?? "invalid response")")
    }
  }

  private struct NetworkPolicyTargets {
    var allowCIDRs: [String] = []
    var allowDomains: [String] = []
    var l7CIDRs: [String] = []
    var l7Domains: [String] = []
    var denyCIDRs: [String] = []
  }

  private func applyNetworkPolicy(
    _ virtualMachine: ManagedVM,
    allowInternetAccess: Bool,
    network: SandboxNetworkRequest?
  ) async throws {
    let targets = try networkPolicyTargets(network: network)
    let policyBody = try JSONEncoder().encode(["rules": network?.rules ?? []])
    let policyResponse = try await guestHTTPRequest(
      virtualMachine,
      method: "POST",
      path: "/files?path=%2Frun%2Fcube-vz-egress-policy.json&username=root",
      body: policyBody,
      contentType: "application/octet-stream"
    )
    guard policyResponse.hasPrefix("HTTP/1.1 200") else {
      throw CubeVZError.runtime("L7 network policy upload failed")
    }
    let script = makeNetworkPolicyScript(
      allowInternetAccess: allowInternetAccess,
      targets: targets
    )
    let response = try await guestHTTPRequest(
      virtualMachine,
      method: "POST",
      path: "/files?path=%2Frun%2Fcube-vz-network.sh&username=root",
      body: Data(script.utf8),
      contentType: "application/octet-stream"
    )
    guard response.hasPrefix("HTTP/1.1 200") else {
      let firstLine = response.split(separator: "\r\n", maxSplits: 1).first.map(String.init)
      throw CubeVZError.runtime("network policy upload failed: \(firstLine ?? "invalid response")")
    }
    let result = try await virtualMachine.executeControlCommand("APPLY_NETWORK")
    guard result == "OK\n" else {
      throw CubeVZError.runtime("guest rejected network policy")
    }
  }

  private func networkPolicyTargets(network: SandboxNetworkRequest?) throws -> NetworkPolicyTargets
  {
    var targets = NetworkPolicyTargets()
    for target in network?.allowOut ?? [] {
      if let cidr = try normalizeIPv4CIDR(target) {
        targets.allowCIDRs.append(cidr)
      } else {
        targets.allowDomains.append(try normalizeDomain(target))
      }
    }
    for target in network?.denyOut ?? [] {
      guard let cidr = try normalizeIPv4CIDR(target) else {
        throw CubeVZError.invalidArguments("denyOut only accepts IPv4 or CIDR: \(target)")
      }
      targets.denyCIDRs.append(cidr)
    }
    for rule in network?.rules ?? [] {
      let ruleTargets = [rule.match.host, rule.match.sni].compactMap({ $0 })
      if ruleTargets.isEmpty {
        targets.l7CIDRs.append("0.0.0.0/0")
      }
      for target in ruleTargets {
        if let cidr = try normalizeIPv4CIDR(target) {
          targets.l7CIDRs.append(cidr)
        } else {
          targets.l7Domains.append(try normalizeDomain(target))
        }
      }
      if let audit = rule.action.audit, !["full", "metadata", "none"].contains(audit) {
        throw CubeVZError.invalidArguments("network rule audit must be full, metadata, or none")
      }
      for injection in rule.action.inject ?? [] {
        guard
          injection.header.range(
            of: "^[A-Za-z0-9!#$%&'*+.^_`|~-]+$",
            options: .regularExpression
          ) != nil
        else {
          throw CubeVZError.invalidArguments("invalid injected header: \(injection.header)")
        }
      }
    }
    let l7CIDRSet = Set(targets.l7CIDRs)
    let l7DomainSet = Set(targets.l7Domains)
    targets.allowCIDRs = targets.allowCIDRs.filter { !l7CIDRSet.contains($0) }
    targets.allowDomains = targets.allowDomains.filter { !l7DomainSet.contains($0) }
    targets.allowCIDRs = Array(Set(targets.allowCIDRs)).sorted()
    targets.allowDomains = Array(Set(targets.allowDomains)).sorted()
    targets.l7CIDRs = Array(l7CIDRSet).sorted()
    targets.l7Domains = Array(l7DomainSet).sorted()
    targets.denyCIDRs = Array(Set(targets.denyCIDRs)).sorted()
    return targets
  }

  private func normalizeIPv4CIDR(_ value: String) throws -> String? {
    let parts = value.split(separator: "/", maxSplits: 1, omittingEmptySubsequences: false)
    guard parts.count <= 2 else {
      throw CubeVZError.invalidArguments("invalid network target: \(value)")
    }
    let address = String(parts[0])
    var parsed = in_addr()
    let isIPv4 = address.withCString { inet_pton(AF_INET, $0, &parsed) == 1 }
    if !isIPv4 { return nil }
    let prefix: Int
    if parts.count == 2 {
      guard let parsedPrefix = Int(parts[1]), (0...32).contains(parsedPrefix) else {
        throw CubeVZError.invalidArguments("invalid IPv4 prefix: \(value)")
      }
      prefix = parsedPrefix
    } else {
      prefix = 32
    }
    return "\(address)/\(prefix)"
  }

  private func normalizeDomain(_ value: String) throws -> String {
    let normalized = value.lowercased().trimmingCharacters(in: .whitespacesAndNewlines)
    guard normalized.count <= 253,
      normalized.range(
        of:
          "^(?:\\*\\.)?(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$",
        options: .regularExpression
      ) != nil
    else {
      throw CubeVZError.invalidArguments("invalid allowOut domain: \(value)")
    }
    return normalized
  }

  private func makeNetworkPolicyScript(
    allowInternetAccess: Bool,
    targets: NetworkPolicyTargets
  ) -> String {
    var lines = [
      "#!/bin/sh",
      "set -eu",
      "modprobe ip_tables 2>/dev/null || true",
      "modprobe ip_set 2>/dev/null || true",
      "modprobe ip_set_hash_ip 2>/dev/null || true",
      "modprobe xt_set 2>/dev/null || true",
      "modprobe xt_owner 2>/dev/null || true",
      "iptables -w -N CUBEVZ_EGRESS 2>/dev/null || true",
      "iptables -w -F CUBEVZ_EGRESS",
      "iptables -w -C OUTPUT -j CUBEVZ_EGRESS 2>/dev/null || iptables -w -I OUTPUT 1 -j CUBEVZ_EGRESS",
      "iptables -w -A CUBEVZ_EGRESS -o lo -j ACCEPT",
    ]

    let allDomains = targets.allowDomains + targets.l7Domains
    if !allDomains.isEmpty {
      lines.append(contentsOf: [
        "ipset create cube_vz_allow hash:ip family inet -exist",
        "ipset flush cube_vz_allow",
        "ipset create cube_vz_l7 hash:ip family inet -exist",
        "ipset flush cube_vz_l7",
        "if [ ! -s /run/cube-vz-upstream-dns ]; then awk '/^nameserver / { print $2; exit }' /etc/resolv.conf > /run/cube-vz-upstream-dns; fi",
        "UPSTREAM_DNS=\"$(cat /run/cube-vz-upstream-dns 2>/dev/null || true)\"",
        "[ -n \"$UPSTREAM_DNS\" ] || UPSTREAM_DNS=192.168.64.1",
        "mkdir -p /etc/dnsmasq.d",
        "cat > /etc/dnsmasq.d/cube-vz.conf <<EOF",
        "no-resolv",
        "server=$UPSTREAM_DNS",
        "listen-address=127.0.0.1",
        "bind-interfaces",
      ])
      for domain in targets.allowDomains {
        let dnsmasqDomain = domain.hasPrefix("*.") ? String(domain.dropFirst(2)) : domain
        lines.append("ipset=/\(dnsmasqDomain)/cube_vz_allow")
      }
      for domain in targets.l7Domains {
        let dnsmasqDomain = domain.hasPrefix("*.") ? String(domain.dropFirst(2)) : domain
        lines.append("ipset=/\(dnsmasqDomain)/cube_vz_l7")
      }
      lines.append(contentsOf: [
        "EOF",
        "if [ -f /run/cube-vz-dnsmasq.pid ]; then kill \"$(cat /run/cube-vz-dnsmasq.pid)\" 2>/dev/null || true; fi",
        "dnsmasq --conf-file=/etc/dnsmasq.d/cube-vz.conf --pid-file=/run/cube-vz-dnsmasq.pid",
        "printf 'nameserver 127.0.0.1\\n' > /etc/resolv.conf",
        "iptables -w -A CUBEVZ_EGRESS -m owner --uid-owner 8049 -m set --match-set cube_vz_l7 dst -j ACCEPT",
        "iptables -w -A CUBEVZ_EGRESS -m set --match-set cube_vz_allow dst -j ACCEPT",
        "iptables -w -A CUBEVZ_EGRESS -m set --match-set cube_vz_l7 dst -j REJECT",
      ])
    } else {
      lines.append(contentsOf: [
        "if [ -f /run/cube-vz-dnsmasq.pid ]; then kill \"$(cat /run/cube-vz-dnsmasq.pid)\" 2>/dev/null || true; rm -f /run/cube-vz-dnsmasq.pid; fi",
        "if [ -s /run/cube-vz-upstream-dns ]; then printf 'nameserver %s\\n' \"$(cat /run/cube-vz-upstream-dns)\" > /etc/resolv.conf; fi",
      ])
    }
    for cidr in targets.allowCIDRs {
      lines.append("iptables -w -A CUBEVZ_EGRESS -d \(cidr) -j ACCEPT")
    }
    for cidr in targets.l7CIDRs {
      lines.append("iptables -w -A CUBEVZ_EGRESS -m owner --uid-owner 8049 -d \(cidr) -j ACCEPT")
      lines.append("iptables -w -A CUBEVZ_EGRESS -d \(cidr) -j REJECT")
    }
    if allowInternetAccess || !allDomains.isEmpty {
      lines.append("iptables -w -A CUBEVZ_EGRESS -p udp --dport 53 -j ACCEPT")
      lines.append("iptables -w -A CUBEVZ_EGRESS -p tcp --dport 53 -j ACCEPT")
    }
    for cidr in targets.denyCIDRs {
      lines.append("iptables -w -A CUBEVZ_EGRESS -d \(cidr) -j REJECT")
    }
    if allowInternetAccess {
      for cidr in [
        "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
        "192.168.0.0/16",
      ] {
        lines.append("iptables -w -A CUBEVZ_EGRESS -d \(cidr) -j REJECT")
      }
      lines.append("iptables -w -A CUBEVZ_EGRESS -j RETURN")
    } else {
      lines.append("iptables -w -A CUBEVZ_EGRESS -j REJECT")
    }
    return lines.joined(separator: "\n") + "\n"
  }

  private func guestHTTPRequest(
    _ virtualMachine: ManagedVM,
    method: String,
    path: String,
    body: Data,
    contentType: String
  ) async throws -> String {
    let connection = try await virtualMachine.connect(toGuestPort: 49_983)
    return try await Task.detached(priority: .userInitiated) {
      let descriptor = connection.fileDescriptor
      let header = Data(
        "\(method) \(path) HTTP/1.1\r\nHost: 127.0.0.1:49983\r\nContent-Type: \(contentType)\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
          .utf8
      )
      try Self.writeAll(header + body, to: descriptor)
      Darwin.shutdown(descriptor, SHUT_WR)
      var response = Data()
      var buffer = [UInt8](repeating: 0, count: 4_096)
      while response.count < 1_048_576 {
        var pollDescriptor = pollfd(fd: descriptor, events: Int16(POLLIN), revents: 0)
        let ready = Darwin.poll(&pollDescriptor, 1, 5_000)
        if ready == 0 { throw CubeVZError.runtime("envd response timed out") }
        if ready < 0 {
          if errno == EINTR { continue }
          throw CubeVZError.runtime("envd poll failed: \(String(cString: strerror(errno)))")
        }
        let count = buffer.withUnsafeMutableBytes {
          Darwin.read(descriptor, $0.baseAddress, $0.count)
        }
        if count == 0 { break }
        guard count > 0 else {
          if errno == EINTR { continue }
          throw CubeVZError.runtime("envd read failed: \(String(cString: strerror(errno)))")
        }
        response.append(contentsOf: buffer.prefix(count))
      }
      connection.close()
      return String(decoding: response, as: UTF8.self)
    }.value
  }

  nonisolated private static func writeAll(_ data: Data, to descriptor: Int32) throws {
    try data.withUnsafeBytes { bytes in
      var offset = 0
      while offset < bytes.count {
        let written = Darwin.write(
          descriptor,
          bytes.baseAddress?.advanced(by: offset),
          bytes.count - offset
        )
        if written < 0, errno == EINTR { continue }
        guard written > 0 else {
          throw CubeVZError.runtime("envd write failed: \(String(cString: strerror(errno)))")
        }
        offset += written
      }
    }
  }

  nonisolated private static func dateString(_ date: Date) -> String {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter.string(from: date)
  }

  private static func expirationDate(timeout: Int?) -> Date? {
    guard let timeout, timeout >= 0 else { return nil }
    return Date().addingTimeInterval(TimeInterval(timeout))
  }

  private static func log(line: String) -> SandboxLog {
    SandboxLog(timestamp: dateString(Date()), line: line)
  }

  private static func diskSizeMiB(directory: VMDirectory, manifest: VMManifest) -> Int? {
    guard
      let attributes = try? FileManager.default.attributesOfItem(
        atPath: directory.fileURL(named: manifest.diskFile).path
      ),
      let size = attributes[.size] as? NSNumber
    else { return nil }
    return Int((size.int64Value + 1_048_575) / 1_048_576)
  }
}
