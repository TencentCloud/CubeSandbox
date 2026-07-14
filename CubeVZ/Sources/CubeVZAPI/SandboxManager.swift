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
  let volumeMounts: [SandboxVolumeMountRequest]?

  private enum CodingKeys: String, CodingKey {
    case templateID, timeout, lifecycle, secure, allowInternetAccess, allow_internet_access
    case network, metadata, distributionScope, distribution_scope, envVars, envs, volumeMounts
  }

  init(
    templateID: String,
    timeout: Int? = nil,
    lifecycle: SandboxLifecycleRequest? = nil,
    secure: Bool? = nil,
    allowInternetAccess: Bool? = nil,
    network: SandboxNetworkRequest? = nil,
    metadata: [String: String]? = nil,
    distributionScope: [String]? = nil,
    envVars: [String: String]? = nil,
    volumeMounts: [SandboxVolumeMountRequest]? = nil
  ) {
    self.templateID = templateID
    self.timeout = timeout
    self.lifecycle = lifecycle
    self.secure = secure
    allow_internet_access = allowInternetAccess
    self.network = network
    self.metadata = metadata
    self.distributionScope = distributionScope
    self.envVars = envVars
    self.volumeMounts = volumeMounts
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
    volumeMounts = try container.decodeIfPresent(
      [SandboxVolumeMountRequest].self,
      forKey: .volumeMounts
    )
  }
}

struct SandboxVolumeMountRequest: Codable, Equatable {
  let name: String
  let path: String
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

struct SandboxLog: Codable {
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

struct TemplateBuildLogsResponse: Encodable {
  let logs: [String]
}

struct CreateTemplateRequest: Decodable, Sendable {
  let image: String
  let instanceType: String?
  let writableLayerSize: String?
  let exposedPorts: [UInt16]?
  let probePort: UInt16?
  let probePath: String?
  let cpu: UInt32?
  let memory: UInt32?
  let env: [String]?
  let allowInternetAccess: Bool?
  let networkType: String?
  let nodes: [String]?
  let registryUsername: String?
  let registryPassword: String?
  let command: [String]?
  let args: [String]?
  let dns: [String]?
  let allowOut: [String]?
  let denyOut: [String]?
}

private struct PersistedTemplate: Codable {
  let templateID: String
  let image: String
  let instanceType: String
  let writableLayerSize: String?
  let exposedPorts: [UInt16]
  let probePort: UInt16?
  let probePath: String?
  let cpu: UInt32?
  let memory: UInt32?
  let env: [String]
  let allowInternetAccess: Bool
  let networkType: String
  let nodes: [String]
  let command: [String]
  let args: [String]
  let dns: [String]
  let allowOut: [String]
  let denyOut: [String]
  let status: String
  let lastError: String?
  let createdAt: Date
  let jobID: String
}

private struct PersistedTemplateBuild: Codable {
  let buildID: String
  let templateID: String
  let status: String
  let phase: String
  let progress: Int
  let message: String
  let logs: [String]
}

private struct PersistedSandbox: Codable {
  let sandboxID: String
  let templateID: String
  let startedAt: Date
  let timeout: Int?
  let metadata: [String: String]?
  let envVars: [String: String]?
  let volumeMounts: [SandboxVolumeMountRequest]?
  let launchTemplateProcess: Bool?
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

private struct PersistedSnapshot: Codable {
  let snapshotID: String
  let names: [String]
  let originSandboxID: String
  let createdAt: Date
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
  let volumeMounts: [SandboxVolumeMountRequest]?
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

struct SandboxMetricsResponse: Encodable {
  let sandboxID: String
  let state: String
  let uptimeSeconds: Int
  let cpuCount: Int
  let memoryMB: Int
  let diskSizeMB: Int?
  let expiresAt: String?
}

struct GuestCommandResult: Encodable {
  let exitCode: Int
  let stdout: String
  let stderr: String
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
    let volumeMounts: [SandboxVolumeMountRequest]?
    let launchTemplateProcess: Bool
    var network: SandboxNetworkRequest?
    var allowInternetAccess: Bool
    var trafficAccessToken: String?
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

  private struct TemplateRecord {
    let templateID: String
    var request: CreateTemplateRequest
    var status: String
    var lastError: String?
    let createdAt: Date
    var jobID: String
  }

  private struct TemplateBuildRecord {
    let buildID: String
    let templateID: String
    var status: String
    var phase: String
    var progress: Int
    var message: String
    var logs: [String]
  }

  private struct GuestBuildResult: Sendable {
    let exitCode: Int32
    let logLines: [String]
    let artifactsDirectory: URL
  }

  private struct ImageRuntime: Sendable {
    let command: [String]
    let args: [String]
    let environment: [String]
    let workingDirectory: String
  }

  private let templateID: String
  private let templateDirectory: VMDirectory
  private let baseTemplateCreatedAt: Date
  private let sandboxesDirectory: URL
  private let snapshotsDirectory: URL
  private let volumesDirectory: URL
  private let templatesDirectory: URL
  private let guestBuilderDirectory: URL?
  private var sandboxes: [String: SandboxRecord] = [:]
  private var snapshots: [String: SnapshotRecord] = [:]
  private var templates: [String: TemplateRecord] = [:]
  private var templateBuilds: [String: TemplateBuildRecord] = [:]
  private var expirationTasks: [String: Task<Void, Never>] = [:]
  private var sandboxLogs: [String: [SandboxLog]] = [:]

  init(
    templateID: String,
    templateDirectory: URL,
    sandboxesDirectory: URL,
    guestBuilderDirectory: URL? = nil
  ) throws {
    self.templateID = templateID
    self.templateDirectory = VMDirectory(url: templateDirectory)
    let templateAttributes = try? FileManager.default.attributesOfItem(
      atPath: self.templateDirectory.manifestURL.path
    )
    baseTemplateCreatedAt = (templateAttributes?[.creationDate] as? Date)
      ?? (templateAttributes?[.modificationDate] as? Date)
      ?? Date()
    self.sandboxesDirectory = sandboxesDirectory
    snapshotsDirectory = sandboxesDirectory.deletingLastPathComponent().appendingPathComponent(
      "snapshots",
      isDirectory: true
    )
    volumesDirectory = sandboxesDirectory.deletingLastPathComponent().appendingPathComponent(
      "volumes",
      isDirectory: true
    )
    templatesDirectory = sandboxesDirectory.deletingLastPathComponent().appendingPathComponent(
      "templates",
      isDirectory: true
    )
    self.guestBuilderDirectory = guestBuilderDirectory

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
    try FileManager.default.createDirectory(
      at: volumesDirectory,
      withIntermediateDirectories: true
    )
    try FileManager.default.createDirectory(
      at: templatesDirectory,
      withIntermediateDirectories: true
    )
    loadPersistedTemplates()
    loadPersistedSnapshots()
    loadPersistedSandboxes()
  }

  func create(request: CreateSandboxRequest) async throws -> SandboxResponse {
    let source: VMDirectory
    let launchTemplateProcess: Bool
    var templateDefaults: TemplateRecord?
    if request.templateID == templateID {
      source = templateDirectory
      launchTemplateProcess = false
    } else if let snapshot = snapshots[request.templateID] {
      source = snapshot.directory
      launchTemplateProcess = false
    } else if templates[request.templateID] != nil,
      let prepared = preparedTemplateDirectory(templateID: request.templateID)
    {
      source = prepared
      launchTemplateProcess = true
      templateDefaults = templates[request.templateID]
    } else {
      throw CubeVZError.invalidArguments("unknown templateID: \(request.templateID)")
    }
    try validate(request: request)
    let effectiveEnvVars = Self.mergedEnvironment(
      template: templateDefaults?.request.env,
      sandbox: request.envVars
    )
    let templateNetwork = templateDefaults.map { template in
      SandboxNetworkRequest(
        allowPublicTraffic: nil,
        allowOut: template.request.allowOut,
        denyOut: template.request.denyOut,
        maskRequestHost: nil,
        rules: nil
      )
    }
    let effectiveNetwork = request.network ?? templateNetwork
    let effectiveInternetAccess = request.allow_internet_access
      ?? templateDefaults?.request.allowInternetAccess
      ?? true
    _ = try networkPolicyTargets(network: effectiveNetwork)

    let sandboxID = "sb-\(UUID().uuidString.lowercased())"
    let destination = sandboxesDirectory.appendingPathComponent(sandboxID, isDirectory: true)
    let directory = try VMTemplateCloner.clone(template: source, to: destination)
    var virtualMachine: ManagedVM?

    do {
      let manifest = try directory.loadManifest()
      let volumeURLs = try volumeDirectoryURLs(
        for: request.volumeMounts,
        directory: directory,
        manifest: manifest
      )
      let createdVirtualMachine = try ManagedVM(
        directory: directory,
        manifest: manifest,
        volumeDirectoryURLs: volumeURLs
      )
      virtualMachine = createdVirtualMachine
      _ = try await createdVirtualMachine.start()
      try await createdVirtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        createdVirtualMachine,
        envVars: effectiveEnvVars,
        network: effectiveNetwork
      )
      try await applyVolumeMounts(
        createdVirtualMachine,
        mounts: request.volumeMounts,
        availableSlots: manifest.volumeShareSlots ?? 0
      )
      try await applyNetworkPolicy(
        createdVirtualMachine,
        allowInternetAccess: effectiveInternetAccess,
        network: effectiveNetwork
      )
      if launchTemplateProcess {
        try await startTemplateProcess(createdVirtualMachine)
      }

      let trafficAccessToken =
        effectiveNetwork?.allowPublicTraffic == false ? UUID().uuidString.lowercased() : nil
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
        envVars: effectiveEnvVars,
        volumeMounts: request.volumeMounts,
        launchTemplateProcess: launchTemplateProcess,
        network: effectiveNetwork,
        allowInternetAccess: effectiveInternetAccess,
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
      persistLogs(sandboxID)
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

  /// Writes a root-owned file through envd's authenticated in-guest file API.
  /// AgentHub uses this to persist its OpenClaw configuration without exposing
  /// another host-side mount or weakening the guest boundary.
  func writeGuestFile(sandboxID: String, path: String, contents: Data) async throws {
    guard let sandbox = sandboxes[sandboxID], sandbox.state == .running,
      let virtualMachine = sandbox.virtualMachine
    else {
      throw CubeVZError.runtime("sandbox is not running: \(sandboxID)")
    }
    guard path.hasPrefix("/"), !path.contains("\0") else {
      throw CubeVZError.invalidArguments("guest file path must be absolute")
    }
    let encodedPath = path.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed)
      ?? path
    let response = try await guestHTTPRequest(
      virtualMachine,
      method: "POST",
      path: "/files?path=\(encodedPath)&username=root",
      body: contents,
      contentType: "application/octet-stream"
    )
    guard response.hasPrefix("HTTP/1.1 200") else {
      throw CubeVZError.runtime("guest file upload failed: \(firstHTTPLine(response))")
    }
  }

  /// Runs a bounded shell command using the same E2B data-plane execution
  /// service exposed to SDK clients. It is intentionally scoped to an already
  /// owned sandbox and never creates a host process.
  func executeGuestShell(
    sandboxID: String,
    script: String,
    timeoutSeconds: Int = 60
  ) async throws -> GuestCommandResult {
    guard let sandbox = sandboxes[sandboxID], sandbox.state == .running,
      let virtualMachine = sandbox.virtualMachine
    else {
      throw CubeVZError.runtime("sandbox is not running: \(sandboxID)")
    }
    let payload: [String: Any] = [
      "code": script,
      "language": "shell",
      "cwd": "/root",
      "timeout": max(1, min(timeoutSeconds, 300)),
    ]
    let body = try JSONSerialization.data(withJSONObject: payload)
    let response = try await guestHTTPRequest(
      virtualMachine,
      guestPort: 49_999,
      method: "POST",
      path: "/execute",
      body: body,
      contentType: "application/json"
    )
    guard response.hasPrefix("HTTP/1.1 200") else {
      throw CubeVZError.runtime("guest command request failed: \(firstHTTPLine(response))")
    }
    return try Self.commandResult(from: response)
  }

  func probeGuestHTTP(sandboxID: String, port: UInt16, path: String = "/") async -> Bool {
    guard let sandbox = sandboxes[sandboxID], sandbox.state == .running,
      let virtualMachine = sandbox.virtualMachine
    else { return false }
    do {
      if port != 49_983 && port != 49_999 {
        let forward = try await virtualMachine.executeControlCommand("FORWARD \(port)")
        guard forward == "OK\n" else { return false }
      }
      let response = try await guestHTTPRequest(
        virtualMachine,
        guestPort: UInt32(port),
        method: "GET",
        path: path,
        body: Data(),
        contentType: "application/json"
      )
      return response.hasPrefix("HTTP/1.1 2") || response.hasPrefix("HTTP/1.1 3")
    } catch {
      return false
    }
  }

  /// Clone a durable named volume for a higher-level service such as AgentHub.
  /// The host filesystem handles the copy and can preserve APFS copy-on-write
  /// behavior for its individual files.
  func cloneNamedVolume(sourceName: String, destinationName: String) throws {
    try validateVolumeMounts([
      SandboxVolumeMountRequest(name: sourceName, path: "/volume-source"),
      SandboxVolumeMountRequest(name: destinationName, path: "/volume-destination"),
    ])
    let source = volumesDirectory.appendingPathComponent(sourceName, isDirectory: true)
    let destination = volumesDirectory.appendingPathComponent(destinationName, isDirectory: true)
    guard FileManager.default.fileExists(atPath: source.path) else {
      throw CubeVZError.runtime("volume not found: \(sourceName)")
    }
    guard !FileManager.default.fileExists(atPath: destination.path) else {
      throw CubeVZError.runtime("volume already exists: \(destinationName)")
    }
    try FileManager.default.copyItem(at: source, to: destination)
  }

  /// Restore a mounted named volume in place. Replacing the directory itself
  /// would leave an active virtiofs share pointing at the old vnode, so this
  /// deliberately replaces its children while retaining the mount root.
  func restoreNamedVolume(snapshotName: String, destinationName: String) throws {
    try validateVolumeMounts([
      SandboxVolumeMountRequest(name: snapshotName, path: "/volume-source"),
      SandboxVolumeMountRequest(name: destinationName, path: "/volume-destination"),
    ])
    guard snapshotName != destinationName else { return }
    let source = volumesDirectory.appendingPathComponent(snapshotName, isDirectory: true)
    let destination = volumesDirectory.appendingPathComponent(destinationName, isDirectory: true)
    guard FileManager.default.fileExists(atPath: source.path) else {
      throw CubeVZError.runtime("volume snapshot not found: \(snapshotName)")
    }
    try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
    let manager = FileManager.default
    for item in try manager.contentsOfDirectory(at: destination, includingPropertiesForKeys: nil) {
      try manager.removeItem(at: item)
    }
    for item in try manager.contentsOfDirectory(at: source, includingPropertiesForKeys: nil) {
      try manager.copyItem(at: item, to: destination.appendingPathComponent(item.lastPathComponent))
    }
  }

  /// Atomically persist service-owned state in a named virtiofs volume.
  /// Relative paths are deliberately constrained below a real (non-symlink)
  /// directory tree so a guest cannot redirect a host-side service write.
  func writeNamedVolumeFile(volumeName: String, relativePath: String, contents: Data) throws {
    try validateVolumeMounts([SandboxVolumeMountRequest(name: volumeName, path: "/volume")])
    let components = relativePath.split(separator: "/").map(String.init)
    guard !components.isEmpty,
      !relativePath.hasPrefix("/"),
      !components.contains(".."),
      !components.contains(".")
    else {
      throw CubeVZError.invalidArguments("volume file path must be a confined relative path")
    }
    let manager = FileManager.default
    let volume = volumesDirectory.appendingPathComponent(volumeName, isDirectory: true)
    try manager.createDirectory(at: volume, withIntermediateDirectories: true)
    try manager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: volume.path)
    var directory = volume
    for component in components.dropLast() {
      let next = directory.appendingPathComponent(component, isDirectory: true)
      if manager.fileExists(atPath: next.path) {
        let values = try next.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey])
        guard values.isDirectory == true, values.isSymbolicLink != true else {
          throw CubeVZError.runtime("volume path contains a non-directory or symlink: \(component)")
        }
      } else {
        try manager.createDirectory(at: next, withIntermediateDirectories: false)
        try manager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: next.path)
      }
      directory = next
    }
    let destination = directory.appendingPathComponent(components.last!)
    if manager.fileExists(atPath: destination.path) {
      let values = try destination.resourceValues(forKeys: [.isSymbolicLinkKey])
      guard values.isSymbolicLink != true else {
        throw CubeVZError.runtime("volume file path is a symlink: \(relativePath)")
      }
    }
    try contents.write(to: destination, options: .atomic)
    try manager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: destination.path)
  }

  func deleteNamedVolume(_ name: String) throws {
    try validateVolumeMounts([SandboxVolumeMountRequest(name: name, path: "/volume")])
    let volume = volumesDirectory.appendingPathComponent(name, isDirectory: true)
    guard FileManager.default.fileExists(atPath: volume.path) else { return }
    try FileManager.default.removeItem(at: volume)
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

  func metrics(sandboxID: String) -> SandboxMetricsResponse? {
    guard let sandbox = sandboxes[sandboxID] else { return nil }
    return SandboxMetricsResponse(
      sandboxID: sandboxID,
      state: sandbox.state.rawValue,
      uptimeSeconds: max(0, Int(Date().timeIntervalSince(sandbox.startedAt))),
      cpuCount: sandbox.cpuCount,
      memoryMB: Int(sandbox.memoryMiB),
      diskSizeMB: sandbox.diskSizeMiB,
      expiresAt: sandbox.expiresAt.map(Self.dateString)
    )
  }

  func updateNetwork(
    sandboxID: String,
    allowInternetAccess: Bool?,
    network: SandboxNetworkRequest?
  ) async throws -> SandboxResponse? {
    guard var sandbox = sandboxes[sandboxID] else { return nil }
    _ = try networkPolicyTargets(network: network)
    let effectiveInternetAccess = allowInternetAccess ?? sandbox.allowInternetAccess
    if sandbox.state == .running {
      guard let virtualMachine = sandbox.virtualMachine else {
        throw CubeVZError.runtime("sandbox VM is unavailable: \(sandboxID)")
      }
      try await applyNetworkPolicy(
        virtualMachine,
        allowInternetAccess: effectiveInternetAccess,
        network: network
      )
    } else if sandbox.state != .paused {
      throw CubeVZError.runtime("sandbox is transitioning: \(sandboxID)")
    }
    sandbox.network = network
    sandbox.allowInternetAccess = effectiveInternetAccess
    if network?.allowPublicTraffic == false, sandbox.trafficAccessToken == nil {
      sandbox.trafficAccessToken = UUID().uuidString.lowercased()
    } else if network?.allowPublicTraffic != false {
      sandbox.trafficAccessToken = nil
    }
    sandboxes[sandboxID] = sandbox
    persist(sandbox)
    appendLog(sandboxID, line: "sandbox network policy updated")
    return response(for: sandbox, exposeTrafficToken: true)
  }

  func listTemplates() -> [TemplateInfoResponse] {
    let base = templateInfo(templateID: templateID, createdAt: nil)
    let snapshotTemplates = snapshots.values.sorted { $0.createdAt < $1.createdAt }.map {
      templateInfo(templateID: $0.snapshotID, createdAt: $0.createdAt)
    }
    let builtTemplates = templates.values.sorted { $0.createdAt < $1.createdAt }.map(
      templateInfo(record:)
    )
    return [base] + snapshotTemplates + builtTemplates
  }

  func getTemplate(templateID requestedID: String) -> TemplateInfoResponse? {
    if requestedID == templateID { return templateInfo(templateID: requestedID, createdAt: nil) }
    if let snapshot = snapshots[requestedID] {
      return templateInfo(templateID: requestedID, createdAt: snapshot.createdAt)
    }
    return templates[requestedID].map(templateInfo(record:))
  }

  func createTemplate(request: CreateTemplateRequest) throws -> TemplateBuildJobResponse {
    let templateID = "tpl-\(UUID().uuidString.lowercased())"
    return try queueTemplateBuild(templateID: templateID, request: request)
  }

  func rebuildTemplate(templateID requestedID: String) throws -> TemplateBuildJobResponse? {
    guard let template = templates[requestedID] else { return nil }
    return try queueTemplateBuild(templateID: requestedID, request: template.request)
  }

  func startTemplateBuild(
    templateID requestedID: String,
    buildID: String
  ) throws -> TemplateBuildJobResponse? {
    guard let template = templates[requestedID] else { return nil }
    return try queueTemplateBuild(
      templateID: requestedID,
      request: template.request,
      buildID: buildID
    )
  }

  func templateBuildStatus(
    templateID requestedID: String,
    buildID: String
  ) -> TemplateBuildStatusResponse? {
    guard let build = templateBuilds[buildID], build.templateID == requestedID else { return nil }
    return TemplateBuildStatusResponse(
      buildID: buildID,
      templateID: requestedID,
      status: build.status,
      progress: build.progress,
      message: build.message
    )
  }

  func templateBuildLogs(
    templateID requestedID: String,
    buildID: String,
    offset: Int,
    limit: Int
  ) -> TemplateBuildLogsResponse? {
    guard let build = templateBuilds[buildID], build.templateID == requestedID else { return nil }
    let start = max(0, offset)
    let count = min(max(1, limit), 10_000)
    return TemplateBuildLogsResponse(logs: Array(build.logs.dropFirst(start).prefix(count)))
  }

  func deleteTemplate(templateID requestedID: String) throws -> Bool {
    guard templates[requestedID] != nil else { return false }
    guard !sandboxes.values.contains(where: { $0.templateID == requestedID }) else {
      throw CubeVZError.runtime("template is in use: \(requestedID)")
    }
    let directory = templatesDirectory.appendingPathComponent(requestedID, isDirectory: true)
    if FileManager.default.fileExists(atPath: directory.path) {
      try FileManager.default.removeItem(at: directory)
    }
    templates.removeValue(forKey: requestedID)
    templateBuilds = templateBuilds.filter { $0.value.templateID != requestedID }
    persistTemplates()
    return true
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
    let volumeURLs = try volumeDirectoryURLs(
      for: sandbox.volumeMounts,
      directory: sandbox.directory,
      manifest: manifest
    )
    let virtualMachine = try ManagedVM(
      directory: sandbox.directory,
      manifest: manifest,
      volumeDirectoryURLs: volumeURLs
    )
    do {
      _ = try await virtualMachine.start()
      try await virtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        virtualMachine,
        envVars: sandbox.envVars,
        network: sandbox.network
      )
      try await applyVolumeMounts(
        virtualMachine,
        mounts: sandbox.volumeMounts,
        availableSlots: manifest.volumeShareSlots ?? 0
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
      persist(record)
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
      let volumeURLs = try volumeDirectoryURLs(
        for: sandbox.volumeMounts,
        directory: sandbox.directory,
        manifest: manifest
      )
      let virtualMachine = try ManagedVM(
        directory: sandbox.directory,
        manifest: manifest,
        volumeDirectoryURLs: volumeURLs
      )
      _ = try await virtualMachine.start()
      try await virtualMachine.waitUntilReady(timeout: .seconds(10))
      try await initializeEnvd(
        virtualMachine,
        envVars: sandbox.envVars,
        network: sandbox.network
      )
      try await applyVolumeMounts(
        virtualMachine,
        mounts: sandbox.volumeMounts,
        availableSlots: manifest.volumeShareSlots ?? 0
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
      volumeMounts: sandbox.volumeMounts,
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
    try validateVolumeMounts(request.volumeMounts)
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
        volumeMounts: persisted.volumeMounts,
        launchTemplateProcess: persisted.launchTemplateProcess ?? false,
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
      let logsURL = directoryURL.appendingPathComponent("sandbox.logs.json")
      if let data = try? Data(contentsOf: logsURL),
        let logs = try? decoder.decode([SandboxLog].self, from: data)
      {
        sandboxLogs[persisted.sandboxID] = logs
      } else {
        sandboxLogs[persisted.sandboxID] = []
      }
      appendLog(persisted.sandboxID, line: "sandbox recovered")
      scheduleExpiration(for: persisted.sandboxID)
    }
  }

  private func loadPersistedSnapshots() {
    let manager = FileManager.default
    guard let entries = try? manager.contentsOfDirectory(
      at: snapshotsDirectory,
      includingPropertiesForKeys: [.isDirectoryKey],
      options: [.skipsHiddenFiles]
    ) else { return }
    let decoder = JSONDecoder()
    for directoryURL in entries {
      let metadataURL = directoryURL.appendingPathComponent("snapshot.json")
      guard let data = try? Data(contentsOf: metadataURL),
        let persisted = try? decoder.decode(PersistedSnapshot.self, from: data)
      else { continue }
      let directory = VMDirectory(url: directoryURL)
      guard let manifest = try? directory.loadManifest(),
        (try? directory.validateFiles(for: manifest)) != nil,
        manager.fileExists(atPath: directory.stateURL.path)
      else { continue }
      snapshots[persisted.snapshotID] = SnapshotRecord(
        snapshotID: persisted.snapshotID,
        names: persisted.names,
        originSandboxID: persisted.originSandboxID,
        directory: directory,
        createdAt: persisted.createdAt
      )
    }
  }

  private func loadPersistedTemplates() {
    let decoder = JSONDecoder()
    let registryURL = templatesDirectory.appendingPathComponent("registry.json")
    if let data = try? Data(contentsOf: registryURL),
      let persisted = try? decoder.decode([PersistedTemplate].self, from: data)
    {
      for item in persisted {
        var status = item.status
        var lastError = item.lastError
        let directory = VMDirectory(
          url: templatesDirectory.appendingPathComponent(item.templateID, isDirectory: true)
        )
        if status == "building" {
          status = "failed"
          lastError = "template build was interrupted by API restart"
        } else if status == "ready" {
          let valid = (try? directory.loadManifest()).flatMap { manifest in
            try? directory.validateFiles(for: manifest)
          } != nil && FileManager.default.fileExists(atPath: directory.stateURL.path)
          if !valid {
            status = "failed"
            lastError = "prepared template artifacts are missing"
          }
        }
        templates[item.templateID] = TemplateRecord(
          templateID: item.templateID,
          request: CreateTemplateRequest(
            image: item.image,
            instanceType: item.instanceType,
            writableLayerSize: item.writableLayerSize,
            exposedPorts: item.exposedPorts,
            probePort: item.probePort,
            probePath: item.probePath,
            cpu: item.cpu,
            memory: item.memory,
            env: item.env,
            allowInternetAccess: item.allowInternetAccess,
            networkType: item.networkType,
            nodes: item.nodes,
            registryUsername: nil,
            registryPassword: nil,
            command: item.command,
            args: item.args,
            dns: item.dns,
            allowOut: item.allowOut,
            denyOut: item.denyOut
          ),
          status: status,
          lastError: lastError,
          createdAt: item.createdAt,
          jobID: item.jobID
        )
      }
    }

    let buildsURL = templatesDirectory.appendingPathComponent("builds.json")
    if let data = try? Data(contentsOf: buildsURL),
      let persisted = try? decoder.decode([PersistedTemplateBuild].self, from: data)
    {
      for item in persisted {
        let interrupted = item.status == "building"
        templateBuilds[item.buildID] = TemplateBuildRecord(
          buildID: item.buildID,
          templateID: item.templateID,
          status: interrupted ? "failed" : item.status,
          phase: interrupted ? "failed" : item.phase,
          progress: item.progress,
          message: interrupted ? "template build was interrupted by API restart" : item.message,
          logs: item.logs + (interrupted ? ["ERROR: build interrupted by API restart"] : [])
        )
      }
    }
    persistTemplates()
  }

  private func persist(_ sandbox: SandboxRecord) {
    let persisted = PersistedSandbox(
      sandboxID: sandbox.sandboxID,
      templateID: sandbox.templateID,
      startedAt: sandbox.startedAt,
      timeout: sandbox.timeout,
      metadata: sandbox.metadata,
      envVars: sandbox.envVars,
      volumeMounts: sandbox.volumeMounts,
      launchTemplateProcess: sandbox.launchTemplateProcess,
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

  private func persist(_ snapshot: SnapshotRecord) {
    let persisted = PersistedSnapshot(
      snapshotID: snapshot.snapshotID,
      names: snapshot.names,
      originSandboxID: snapshot.originSandboxID,
      createdAt: snapshot.createdAt
    )
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    try? encoder.encode(persisted).write(
      to: snapshot.directory.url.appendingPathComponent("snapshot.json"),
      options: [.atomic]
    )
  }

  private func persistTemplates() {
    let persistedTemplates = templates.values.sorted { $0.templateID < $1.templateID }.map {
      template in
      PersistedTemplate(
        templateID: template.templateID,
        image: template.request.image,
        instanceType: template.request.instanceType ?? "cube-vz",
        writableLayerSize: template.request.writableLayerSize,
        exposedPorts: template.request.exposedPorts ?? [],
        probePort: template.request.probePort,
        probePath: template.request.probePath,
        cpu: template.request.cpu,
        memory: template.request.memory,
        env: template.request.env ?? [],
        allowInternetAccess: template.request.allowInternetAccess ?? true,
        networkType: template.request.networkType ?? "nat",
        nodes: template.request.nodes ?? [],
        command: template.request.command ?? [],
        args: template.request.args ?? [],
        dns: template.request.dns ?? [],
        allowOut: template.request.allowOut ?? [],
        denyOut: template.request.denyOut ?? [],
        status: template.status,
        lastError: template.lastError,
        createdAt: template.createdAt,
        jobID: template.jobID
      )
    }
    let persistedBuilds = templateBuilds.values.sorted { $0.buildID < $1.buildID }.map {
      build in
      PersistedTemplateBuild(
        buildID: build.buildID,
        templateID: build.templateID,
        status: build.status,
        phase: build.phase,
        progress: build.progress,
        message: build.message,
        logs: build.logs
      )
    }
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    try? encoder.encode(persistedTemplates).write(
      to: templatesDirectory.appendingPathComponent("registry.json"),
      options: [.atomic]
    )
    try? encoder.encode(persistedBuilds).write(
      to: templatesDirectory.appendingPathComponent("builds.json"),
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
    persistLogs(sandboxID)
  }

  private func persistLogs(_ sandboxID: String) {
    guard let sandbox = sandboxes[sandboxID], let logs = sandboxLogs[sandboxID] else { return }
    let encoder = JSONEncoder()
    try? encoder.encode(logs).write(
      to: sandbox.directory.url.appendingPathComponent("sandbox.logs.json"),
      options: [.atomic]
    )
  }

  private func templateInfo(templateID requestedID: String, createdAt: Date?) -> TemplateInfoResponse {
    TemplateInfoResponse(
      templateID: requestedID,
      instanceType: "cube-vz",
      version: "native",
      status: "ready",
      lastError: nil,
      createdAt: Self.dateString(createdAt ?? baseTemplateCreatedAt),
      imageInfo: "ARM64 Linux guest on Apple Virtualization.framework",
      jobID: nil,
      networkType: "nat",
      allowInternetAccess: true
    )
  }

  private func templateInfo(record: TemplateRecord) -> TemplateInfoResponse {
    TemplateInfoResponse(
      templateID: record.templateID,
      instanceType: record.request.instanceType ?? "cube-vz",
      version: "native",
      status: record.status,
      lastError: record.lastError,
      createdAt: Self.dateString(record.createdAt),
      imageInfo: record.request.image,
      jobID: record.jobID,
      networkType: record.request.networkType ?? "nat",
      allowInternetAccess: record.request.allowInternetAccess ?? true
    )
  }

  private func preparedTemplateDirectory(templateID requestedID: String) -> VMDirectory? {
    let directory = VMDirectory(
      url: templatesDirectory.appendingPathComponent(requestedID, isDirectory: true)
    )
    guard let manifest = try? directory.loadManifest(),
      (try? directory.validateFiles(for: manifest)) != nil,
      FileManager.default.fileExists(atPath: directory.stateURL.path)
    else { return nil }
    return directory
  }

  private func queueTemplateBuild(
    templateID requestedID: String,
    request: CreateTemplateRequest,
    buildID requestedBuildID: String? = nil
  ) throws -> TemplateBuildJobResponse {
    let image = request.image.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !image.isEmpty else {
      throw CubeVZError.invalidArguments("template image is required")
    }
    try validateTemplateRequest(request)
    if templates[requestedID]?.status == "building" {
      throw CubeVZError.runtime("template build is already running: \(requestedID)")
    }
    _ = try Self.rootfsSizeMiB(request.writableLayerSize)
    let buildID = requestedBuildID ?? "build-\(UUID().uuidString.lowercased())"
    let createdAt = templates[requestedID]?.createdAt ?? Date()
    templates[requestedID] = TemplateRecord(
      templateID: requestedID,
      request: request,
      status: "building",
      lastError: nil,
      createdAt: createdAt,
      jobID: buildID
    )
    templateBuilds[buildID] = TemplateBuildRecord(
      buildID: buildID,
      templateID: requestedID,
      status: "building",
      phase: "queued",
      progress: 0,
      message: "template build queued",
      logs: ["queued ARM64 guest build for \(image)"]
    )
    persistTemplates()
    Task { [weak self] in
      await self?.performTemplateBuild(
        templateID: requestedID,
        buildID: buildID,
        request: request
      )
    }
    return TemplateBuildJobResponse(
      jobID: buildID,
      templateID: requestedID,
      status: "building",
      phase: "queued",
      progress: 0,
      errorMessage: ""
    )
  }

  private func validateTemplateRequest(_ request: CreateTemplateRequest) throws {
    if let cpu = request.cpu, cpu == 0 {
      throw CubeVZError.invalidArguments("template cpu must be greater than zero")
    }
    if let memory = request.memory, memory < 256 {
      throw CubeVZError.invalidArguments("template memory must be at least 256MiB")
    }
    if let port = request.probePort, port == 0 {
      throw CubeVZError.invalidArguments("probePort must be greater than zero")
    }
    if request.exposedPorts?.contains(0) == true {
      throw CubeVZError.invalidArguments("exposedPorts must be greater than zero")
    }
    for entry in request.env ?? [] {
      let parts = entry.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
      guard parts.count == 2, !parts[0].isEmpty, !parts[0].contains("\0") else {
        throw CubeVZError.invalidArguments("template env entries must use KEY=VALUE")
      }
    }
    for value in (request.command ?? []) + (request.args ?? []) {
      guard !value.contains("\0") else {
        throw CubeVZError.invalidArguments("template command contains a NUL byte")
      }
    }
  }

  private func performTemplateBuild(
    templateID requestedID: String,
    buildID: String,
    request: CreateTemplateRequest
  ) async {
    guard let builderDirectory = guestBuilderDirectory else {
      failTemplateBuild(
        templateID: requestedID,
        buildID: buildID,
        message: "guest builder directory is not configured"
      )
      return
    }
    let buildRoot = templatesDirectory.appendingPathComponent(
      ".build-\(buildID)",
      isDirectory: true
    )
    updateTemplateBuild(
      buildID: buildID,
      phase: "building-rootfs",
      progress: 10,
      message: "building ARM64 root filesystem"
    )

    do {
      let result = try await Task.detached(priority: .userInitiated) {
        try Self.runGuestBuild(
          builderDirectory: builderDirectory,
          buildRoot: buildRoot,
          request: request
        )
      }.value
      appendTemplateBuildLogs(buildID: buildID, lines: result.logLines)
      guard result.exitCode == 0 else {
        throw CubeVZError.runtime("Docker guest build exited with code \(result.exitCode)")
      }

      updateTemplateBuild(
        buildID: buildID,
        phase: "preparing-template",
        progress: 75,
        message: "preparing Virtualization.framework saved state"
      )
      let baseManifest = try templateDirectory.loadManifest()
      let stagedURL = templatesDirectory.appendingPathComponent(
        ".\(requestedID).partial-\(buildID)",
        isDirectory: true
      )
      try? FileManager.default.removeItem(at: stagedURL)
      let cpuCount = request.cpu.map { max(1, Int((UInt64($0) + 999) / 1_000)) }
        ?? baseManifest.cpuCount
      let memoryMiB = request.memory.map(UInt64.init) ?? baseManifest.memoryMiB
      let staged = try VMDirectoryCreator.create(
        CreateVMRequest(
          destination: stagedURL,
          kernel: result.artifactsDirectory.appendingPathComponent("kernel"),
          disk: result.artifactsDirectory.appendingPathComponent("rootfs.raw"),
          initrd: result.artifactsDirectory.appendingPathComponent("initrd"),
          cpuCount: cpuCount,
          memoryMiB: memoryMiB,
          commandLine: baseManifest.commandLine,
          networkEnabled: true,
          vsockEnabled: true,
          allowFullCopy: true,
          volumeShareSlots: baseManifest.volumeShareSlots ?? 8
        )
      )
      let virtualMachine = try ManagedVM(
        directory: staged,
        manifest: try staged.loadManifest()
      )
      _ = try await virtualMachine.start(restoreIfPresent: false)
      try await virtualMachine.waitUntilReady(timeout: .seconds(30))
      try await virtualMachine.saveStateAndStop()

      let destination = templatesDirectory.appendingPathComponent(requestedID, isDirectory: true)
      let backup = templatesDirectory.appendingPathComponent(
        ".\(requestedID).backup-\(buildID)",
        isDirectory: true
      )
      try? FileManager.default.removeItem(at: backup)
      if FileManager.default.fileExists(atPath: destination.path) {
        try FileManager.default.moveItem(at: destination, to: backup)
      }
      do {
        try FileManager.default.moveItem(at: stagedURL, to: destination)
        try? FileManager.default.removeItem(at: backup)
      } catch {
        if FileManager.default.fileExists(atPath: backup.path) {
          try? FileManager.default.moveItem(at: backup, to: destination)
        }
        throw error
      }

      guard var template = templates[requestedID] else { return }
      template.status = "ready"
      template.lastError = nil
      templates[requestedID] = template
      updateTemplateBuild(
        buildID: buildID,
        phase: "ready",
        progress: 100,
        status: "completed",
        message: "template is ready"
      )
      appendTemplateBuildLogs(buildID: buildID, lines: ["template saved state is ready"])
      persistTemplates()
      try? FileManager.default.removeItem(at: buildRoot)
    } catch {
      try? FileManager.default.removeItem(at: buildRoot)
      failTemplateBuild(
        templateID: requestedID,
        buildID: buildID,
        message: error.localizedDescription
      )
    }
  }

  private func updateTemplateBuild(
    buildID: String,
    phase: String,
    progress: Int,
    status: String = "building",
    message: String
  ) {
    guard var build = templateBuilds[buildID] else { return }
    build.phase = phase
    build.progress = progress
    build.status = status
    build.message = message
    templateBuilds[buildID] = build
    persistTemplates()
  }

  private func appendTemplateBuildLogs(buildID: String, lines: [String]) {
    guard var build = templateBuilds[buildID] else { return }
    build.logs.append(contentsOf: lines.filter { !$0.isEmpty })
    if build.logs.count > 10_000 { build.logs = Array(build.logs.suffix(10_000)) }
    templateBuilds[buildID] = build
    persistTemplates()
  }

  private func failTemplateBuild(templateID requestedID: String, buildID: String, message: String) {
    if var template = templates[requestedID] {
      template.status = "failed"
      template.lastError = message
      templates[requestedID] = template
    }
    updateTemplateBuild(
      buildID: buildID,
      phase: "failed",
      progress: templateBuilds[buildID]?.progress ?? 0,
      status: "failed",
      message: message
    )
    appendTemplateBuildLogs(buildID: buildID, lines: ["ERROR: \(message)"])
    persistTemplates()
  }

  private func validateVolumeMounts(_ mounts: [SandboxVolumeMountRequest]?) throws {
    guard let mounts else { return }
    guard mounts.count <= 16 else {
      throw CubeVZError.invalidArguments("at most 16 volume mounts are supported")
    }
    var paths = Set<String>()
    for mount in mounts {
      guard
        mount.name.range(
          of: "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$",
          options: .regularExpression
        ) != nil
      else {
        throw CubeVZError.invalidArguments("invalid volume name: \(mount.name)")
      }
      let path = URL(fileURLWithPath: mount.path).standardizedFileURL.path
      guard mount.path.hasPrefix("/"), path != "/", !path.contains("\0") else {
        throw CubeVZError.invalidArguments("volume mount path must be an absolute non-root path")
      }
      let protectedPaths = ["/dev", "/proc", "/sys", "/run", "/mnt/cube-volumes"]
      guard !protectedPaths.contains(where: { path == $0 || path.hasPrefix("\($0)/") }) else {
        throw CubeVZError.invalidArguments("volume mount path is reserved: \(mount.path)")
      }
      guard paths.insert(path).inserted else {
        throw CubeVZError.invalidArguments("duplicate volume mount path: \(mount.path)")
      }
    }
  }

  private func volumeDirectoryURLs(
    for mounts: [SandboxVolumeMountRequest]?,
    directory: VMDirectory,
    manifest: VMManifest
  ) throws -> [URL]? {
    let slots = manifest.volumeShareSlots ?? 0
    let mounts = mounts ?? []
    guard mounts.count <= slots else {
      throw CubeVZError.unsupported(
        "template exposes \(slots) volume slots but \(mounts.count) mounts were requested"
      )
    }
    guard slots > 0 else { return nil }
    var urls: [URL] = []
    for slot in 0..<slots {
      let url: URL
      if slot < mounts.count {
        url = volumesDirectory.appendingPathComponent(mounts[slot].name, isDirectory: true)
      } else {
        url = directory.volumeShareURL(slot: slot)
      }
      try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
      urls.append(url)
    }
    return urls
  }

  private func applyVolumeMounts(
    _ virtualMachine: ManagedVM,
    mounts: [SandboxVolumeMountRequest]?,
    availableSlots: Int
  ) async throws {
    let mounts = mounts ?? []
    guard mounts.count <= availableSlots else {
      throw CubeVZError.unsupported("not enough virtiofs volume slots")
    }
    guard !mounts.isEmpty else { return }
    var lines = ["#!/bin/sh", "set -eu"]
    for (slot, mount) in mounts.enumerated() {
      let source = "/mnt/cube-volumes/\(slot)"
      let target = URL(fileURLWithPath: mount.path).standardizedFileURL.path
      lines.append("mountpoint -q \(Self.shellQuote(source))")
      lines.append("mkdir -p \(Self.shellQuote(target))")
      lines.append("mountpoint -q \(Self.shellQuote(target)) && umount \(Self.shellQuote(target)) || true")
      lines.append("mount --bind \(Self.shellQuote(source)) \(Self.shellQuote(target))")
    }
    let script = lines.joined(separator: "\n") + "\n"
    let response = try await guestHTTPRequest(
      virtualMachine,
      method: "POST",
      path: "/files?path=%2Frun%2Fcube-vz-volumes.sh&username=root",
      body: Data(script.utf8),
      contentType: "application/octet-stream"
    )
    guard response.hasPrefix("HTTP/1.1 200") else {
      throw CubeVZError.runtime("volume mount script upload failed")
    }
    let result = try await virtualMachine.executeControlCommand("APPLY_VOLUMES")
    guard result == "OK\n" else {
      throw CubeVZError.runtime("guest rejected volume mounts")
    }
  }

  private func startTemplateProcess(_ virtualMachine: ManagedVM) async throws {
    let result = try await virtualMachine.executeControlCommand("START_TEMPLATE")
    guard result == "OK\n" else {
      throw CubeVZError.runtime("guest failed to start the template command")
    }
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

  private func firstHTTPLine(_ response: String) -> String {
    response.split(separator: "\n", maxSplits: 1).first.map(String.init) ?? "invalid response"
  }

  nonisolated private static func commandResult(from response: String) throws -> GuestCommandResult {
    guard let separator = response.range(of: "\r\n\r\n") else {
      throw CubeVZError.runtime("guest command returned malformed HTTP response")
    }
    let events = response[separator.upperBound...]
    var stdout = ""
    var stderr = ""
    var exitCode = 0
    for line in events.split(separator: "\n") where !line.isEmpty {
      guard let data = String(line).data(using: .utf8),
        let event = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
        let type = event["type"] as? String
      else { continue }
      switch type {
      case "stdout":
        stdout += event["text"] as? String ?? ""
      case "stderr":
        stderr += event["text"] as? String ?? ""
      case "error":
        let name = event["name"] as? String ?? "GuestError"
        let value = event["value"] as? String ?? "unknown guest error"
        if !stderr.isEmpty, !stderr.hasSuffix("\n") { stderr += "\n" }
        stderr += "\(name): \(value)\n"
        exitCode = max(exitCode, 1)
      case "result":
        if let extra = event["extra"] as? [String: Any], let code = extra["exit_code"] as? Int {
          exitCode = code
        }
      default:
        continue
      }
    }
    return GuestCommandResult(exitCode: exitCode, stdout: stdout, stderr: stderr)
  }

  private func guestHTTPRequest(
    _ virtualMachine: ManagedVM,
    guestPort: UInt32 = 49_983,
    method: String,
    path: String,
    body: Data,
    contentType: String
  ) async throws -> String {
    let connection = try await virtualMachine.connect(toGuestPort: guestPort)
    return try await Task.detached(priority: .userInitiated) {
      let descriptor = connection.fileDescriptor
      let header = Data(
        "\(method) \(path) HTTP/1.1\r\nHost: 127.0.0.1:\(guestPort)\r\nContent-Type: \(contentType)\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
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

  nonisolated private static func runGuestBuild(
    builderDirectory: URL,
    buildRoot: URL,
    request: CreateTemplateRequest
  ) throws -> GuestBuildResult {
    let manager = FileManager.default
    let script = builderDirectory.appendingPathComponent("build-guest.sh")
    guard manager.isExecutableFile(atPath: script.path) else {
      throw CubeVZError.filesystem("guest build script is unavailable: \(script.path)")
    }
    try? manager.removeItem(at: buildRoot)
    try manager.createDirectory(at: buildRoot, withIntermediateDirectories: true)
    let artifacts = buildRoot.appendingPathComponent("artifacts", isDirectory: true)
    let logURL = buildRoot.appendingPathComponent("build.log")
    guard manager.createFile(atPath: logURL.path, contents: nil) else {
      throw CubeVZError.filesystem("cannot create template build log")
    }
    let logHandle = try FileHandle(forWritingTo: logURL)
    defer { try? logHandle.close() }

    var environment = ProcessInfo.processInfo.environment
    environment["CUBEVZ_BENCH_ASSET_DIR"] = artifacts.path
    environment["CUBEVZ_TEMPLATE_IMAGE"] = request.image
    environment["CUBEVZ_ROOTFS_SIZE_MIB"] = String(
      try rootfsSizeMiB(request.writableLayerSize)
    )

    if let username = request.registryUsername, !username.isEmpty,
      let password = request.registryPassword, !password.isEmpty
    {
      let dockerConfig = buildRoot.appendingPathComponent("docker-auth", isDirectory: true)
      try manager.createDirectory(at: dockerConfig, withIntermediateDirectories: true)
      let auth = Data("\(username):\(password)".utf8).base64EncodedString()
      let registry = registryHost(for: request.image)
      let config = try JSONSerialization.data(withJSONObject: [
        "auths": [registry: ["auth": auth]]
      ])
      try config.write(to: dockerConfig.appendingPathComponent("config.json"), options: .atomic)
      environment["DOCKER_CONFIG"] = dockerConfig.path
    }

    let imageRuntime = try inspectImageRuntime(
      image: request.image,
      request: request,
      environment: environment,
      logHandle: logHandle
    )
    let runtimeConfig: [String: Any] = [
      "command": imageRuntime.command,
      "args": imageRuntime.args,
      "env": imageRuntime.environment,
      "cwd": imageRuntime.workingDirectory,
      "exposedPorts": request.exposedPorts ?? [],
    ]
    let runtimeData = try JSONSerialization.data(withJSONObject: runtimeConfig)
    environment["CUBEVZ_TEMPLATE_CONFIG_B64"] = runtimeData.base64EncodedString()

    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/bin/bash")
    process.arguments = [script.path]
    process.environment = environment
    process.currentDirectoryURL = builderDirectory
    process.standardOutput = logHandle
    process.standardError = logHandle
    try process.run()
    process.waitUntilExit()
    try logHandle.synchronize()
    let logData = (try? Data(contentsOf: logURL)) ?? Data()
    let logLines = String(decoding: logData, as: UTF8.self)
      .split(whereSeparator: { $0.isNewline })
      .map(String.init)
    return GuestBuildResult(
      exitCode: process.terminationStatus,
      logLines: logLines,
      artifactsDirectory: artifacts
    )
  }

  nonisolated private static func inspectImageRuntime(
    image: String,
    request: CreateTemplateRequest,
    environment: [String: String],
    logHandle: FileHandle
  ) throws -> ImageRuntime {
    let pull = Process()
    pull.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    pull.arguments = ["docker", "pull", "--platform", "linux/arm64", image]
    pull.environment = environment
    pull.standardOutput = logHandle
    pull.standardError = logHandle
    try pull.run()
    pull.waitUntilExit()
    guard pull.terminationStatus == 0 else {
      throw CubeVZError.runtime("cannot pull ARM64 template image: \(image)")
    }

    let output = Pipe()
    let inspect = Process()
    inspect.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    inspect.arguments = ["docker", "image", "inspect", image]
    inspect.environment = environment
    inspect.standardOutput = output
    inspect.standardError = logHandle
    try inspect.run()
    inspect.waitUntilExit()
    let data = output.fileHandleForReading.readDataToEndOfFile()
    guard inspect.terminationStatus == 0,
      let array = try JSONSerialization.jsonObject(with: data) as? [[String: Any]],
      let config = array.first?["Config"] as? [String: Any]
    else {
      throw CubeVZError.runtime("cannot inspect template image configuration")
    }

    let entrypoint = config["Entrypoint"] as? [String] ?? []
    let imageCommand = config["Cmd"] as? [String] ?? []
    let command: [String]
    let args: [String]
    if let requestedCommand = request.command, !requestedCommand.isEmpty {
      command = requestedCommand
      args = request.args ?? imageCommand
    } else if !entrypoint.isEmpty {
      command = entrypoint
      args = request.args ?? imageCommand
    } else {
      command = imageCommand
      args = request.args ?? []
    }
    return ImageRuntime(
      command: command,
      args: args,
      environment: mergeEnvironment(
        base: config["Env"] as? [String] ?? [],
        overrides: request.env ?? []
      ),
      workingDirectory: (config["WorkingDir"] as? String).flatMap { $0.isEmpty ? nil : $0 }
        ?? "/"
    )
  }

  nonisolated private static func mergeEnvironment(
    base: [String],
    overrides: [String]
  ) -> [String] {
    var order: [String] = []
    var values: [String: String] = [:]
    for entry in base + overrides {
      let parts = entry.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
      guard parts.count == 2, !parts[0].isEmpty else { continue }
      let key = String(parts[0])
      if values[key] == nil { order.append(key) }
      values[key] = String(parts[1])
    }
    return order.compactMap { key in values[key].map { "\(key)=\($0)" } }
  }

  nonisolated private static func mergedEnvironment(
    template: [String]?,
    sandbox: [String: String]?
  ) -> [String: String]? {
    var result: [String: String] = [:]
    for entry in template ?? [] {
      let parts = entry.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
      guard parts.count == 2, !parts[0].isEmpty else { continue }
      result[String(parts[0])] = String(parts[1])
    }
    for (key, value) in sandbox ?? [:] { result[key] = value }
    return result.isEmpty ? nil : result
  }

  nonisolated private static func rootfsSizeMiB(_ value: String?) throws -> Int {
    guard let value, !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
      return 768
    }
    let normalized = value.trimmingCharacters(in: .whitespacesAndNewlines).uppercased()
    let suffixes: [(String, Int)] = [
      ("GIB", 1_024), ("GB", 1_024), ("G", 1_024),
      ("MIB", 1), ("MB", 1), ("M", 1),
    ]
    var number = normalized
    var multiplier = 1
    if let match = suffixes.first(where: { normalized.hasSuffix($0.0) }) {
      number = String(normalized.dropLast(match.0.count))
      multiplier = match.1
    }
    guard let amount = Double(number), amount > 0 else {
      throw CubeVZError.invalidArguments("invalid writableLayerSize: \(value)")
    }
    let result = Int(ceil(amount * Double(multiplier)))
    guard (256...32_768).contains(result) else {
      throw CubeVZError.invalidArguments(
        "writableLayerSize must resolve to between 256MiB and 32GiB"
      )
    }
    return result
  }

  nonisolated private static func registryHost(for image: String) -> String {
    let first = image.split(separator: "/", maxSplits: 1).first.map(String.init) ?? ""
    if first == "localhost" || first.contains(".") || first.contains(":") {
      return first
    }
    return "https://index.docker.io/v1/"
  }

  nonisolated private static func shellQuote(_ value: String) -> String {
    "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
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
