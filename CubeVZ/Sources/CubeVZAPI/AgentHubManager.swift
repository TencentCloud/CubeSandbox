// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

struct CreateAgentInstanceRequest: Decodable {
  let name: String
  let engine: String
  let model: String?
  let templateId: String?
  let snapshotId: String?
  let persistenceMode: String?
  let botId: String?
  let botSecret: String?
}

struct UpdateAgentModelRequest: Decodable {
  let model: String
}

struct UpdateWeComConfigRequest: Decodable {
  let botId: String
  let botSecret: String
}

struct CreateAgentSnapshotRequest: Decodable {
  let name: String?
}

struct RollbackAgentRequest: Decodable {
  let snapshotId: String
}

struct CloneAgentRequest: Decodable {
  let name: String?
  let snapshotId: String?
}

struct PublishAgentTemplateRequest: Decodable {
  let name: String?
  let snapshotId: String?
}

struct RegisterMarketAgentTemplateRequest: Decodable {
  let templateId: String
  let name: String?
  let model: String?
  let version: String?
  let recommended: Bool?
}

struct UpdateAgentTemplateRequest: Decodable {
  let name: String?
  let recommended: Bool?
}

struct UpdateAgentSnapshotRequest: Decodable {
  let name: String?
  let isHealthy: Bool?
}

struct UpdateAgentSettingsRequest: Decodable {
  let deepseekApiKey: String?
  let llmProvider: String?
  let llmBaseUrl: String?
  let llmModel: String?
  let llmApiKey: String?
  let llmCredentialMode: String?
  let gatewayDomain: String?
}

struct AgentWeComConfig: Codable {
  let botId: String
  let botSecret: String
}

struct AgentSetupResult: Codable {
  let exitCode: Int
  let stdout: String
  let stderr: String
}

struct AgentInstanceResponse: Encodable {
  let id: String
  let name: String
  let status: String
  let engine: String
  let env: String
  let model: String
  let version: String
  let bots: [String]
  let botsAvailable: [String]
  let avatar: String
  let avatarTone: String
  let sandboxId: String
  let templateId: String
  let gatewayUrl: String
  let envUrl: String
  let persistenceMode: String?
  let rootfsSourceType: String?
  let rootfsSourceId: String?
  let openclawPersistId: String?
  let openclawStatePath: String?
  let wecomConfig: AgentWeComConfig?
  let setup: AgentSetupResult?
}

struct AgentGatewayHealthResponse: Encodable {
  let ready: Bool
}

struct AgentSnapshotResponse: Encodable {
  let snapshotID: String
  let names: [String]
  let status: String
  let snapshotKind: String?
  let originSandboxID: String?
  let publishedTemplateId: String?
  let rootfsSourceType: String?
  let rootfsSourceId: String?
  let rootfsSnapshotId: String?
  let openclawStateSnapshotPath: String?
  let templateReferenced: Bool
  let isHealthy: Bool
  let parentSnapshotID: String?
  let createdAt: String?
  let updatedAt: String?
}

struct AgentTemplateResponse: Encodable {
  let templateId: String
  let name: String
  let sourceAgentId: String
  let sourceSnapshotId: String
  let sourceSandboxId: String
  let model: String
  let version: String
  let persistenceMode: String?
  let recommended: Bool
  let createdAt: String?
}

struct AgentOperationResponse: Encodable {
  let operationId: String
  let agentId: String
  let operationType: String
  let status: String
  let targetId: String?
  let errorMessage: String?
  let createdAt: String?
  let updatedAt: String?
}

struct AgentSnapshotJobResponse: Encodable {
  let operationId: String?
  let status: String
}

struct AgentRollbackResponse: Encodable {
  let sandboxID: String
  let snapshotID: String
  let operationID: String
  let status: String
}

struct AgentRecoverResponse: Encodable {
  let recovered: Bool
  let method: String
  let snapshotID: String?
}

struct AgentPublishTemplateResponse: Encodable {
  let templateId: String
  let snapshotId: String
  let name: String?
}

struct AgentSettingsResponse: Encodable {
  let deepseekApiKeyConfigured: Bool
  let deepseekApiKeyMasked: String?
  let source: String
  let llmProvider: String
  let llmBaseUrl: String
  let llmModel: String
  let llmApiKeyConfigured: Bool
  let llmApiKeyMasked: String?
  let llmApiKeySource: String
  let llmCredentialMode: String
  let persistenceEnabled: Bool
  let gatewayDomain: String?
}

/// Durable macOS implementation of CubeAPI's AgentHub control plane. The
/// Linux deployment stores equivalent rows in MySQL; CubeVZ keeps its state
/// locally and delegates lifecycle, snapshots, and volumes to SandboxManager.
@MainActor
final class AgentHubManager {
  private static let openClawPort: UInt16 = 18_789
  private static let defaultTimeout = 86_400

  private struct Settings: Codable {
    var apiKey: String?
    var provider: String
    var baseURL: String
    var model: String
    var credentialMode: String
    var gatewayDomain: String?

    static let `default` = Settings(
      apiKey: nil,
      provider: "deepseek",
      baseURL: "https://api.deepseek.com",
      model: "deepseek-chat",
      credentialMode: "egress",
      gatewayDomain: nil
    )
  }

  private struct AgentRecord: Codable {
    let id: String
    var name: String
    var status: String
    let engine: String
    var model: String
    let sandboxID: String
    let templateID: String
    let persistenceMode: String
    let rootfsSourceType: String
    let rootfsSourceID: String
    let volumeName: String?
    let gatewayToken: String
    var wecomConfig: AgentWeComConfig?
    var setup: AgentSetupResult?
    let createdAt: Date
  }

  private struct SnapshotRecord: Codable {
    let snapshotID: String
    let agentID: String
    var names: [String]
    var status: String
    var isHealthy: Bool
    var publishedTemplateID: String?
    /// Optional for compatibility with state written before model capture was
    /// added; newly created snapshots always record it.
    let model: String?
    let wecomConfig: AgentWeComConfig?
    /// Immutable copy of a shared-files OpenClaw volume for this snapshot.
    let volumeSnapshotName: String?
    /// Base guest template to use when the state volume is cloned into a new
    /// sandbox. A VZ saved state retains its old virtiofs attachment, so a
    /// shared-files clone must start from the clean base template instead.
    let sharedFilesTemplateID: String?
    let parentSnapshotID: String?
    let createdAt: Date
    var updatedAt: Date
  }

  private struct TemplateRecord: Codable {
    let templateID: String
    var name: String
    let sourceAgentID: String
    let sourceSnapshotID: String
    let sourceSandboxID: String
    var model: String
    let version: String
    let persistenceMode: String?
    let wecomConfig: AgentWeComConfig?
    /// Optional shared-files state that must be cloned with a published template.
    let volumeSnapshotName: String?
    let sharedFilesTemplateID: String?
    var recommended: Bool
    let createdAt: Date
  }

  private struct OperationRecord: Codable {
    let operationID: String
    let agentID: String
    let operationType: String
    var status: String
    var targetID: String?
    var errorMessage: String?
    let createdAt: Date
    var updatedAt: Date
  }

  private struct State: Codable {
    var settings: Settings
    var agents: [String: AgentRecord]
    var snapshots: [String: SnapshotRecord]
    var templates: [String: TemplateRecord]
    var operations: [String: OperationRecord]
  }

  private let manager: SandboxManager
  private let defaultTemplateID: String
  private let stateURL: URL
  private var state: State

  init(manager: SandboxManager, defaultTemplateID: String, directory: URL) throws {
    self.manager = manager
    self.defaultTemplateID = defaultTemplateID
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    stateURL = directory.appendingPathComponent("agenthub.json")
    if FileManager.default.fileExists(atPath: stateURL.path) {
      let data = try Data(contentsOf: stateURL)
      state = try JSONDecoder().decode(State.self, from: data)
    } else {
      state = State(settings: .default, agents: [:], snapshots: [:], templates: [:], operations: [:])
      persist()
    }
  }

  func listInstances() -> [AgentInstanceResponse] {
    state.agents.values
      .sorted { $0.createdAt < $1.createdAt }
      .map(response(for:))
  }

  func createInstance(_ request: CreateAgentInstanceRequest) async throws -> AgentInstanceResponse {
    try validate(request)
    return try await createInstanceImpl(request)
  }

  func deleteInstance(agentID: String) async throws -> Bool {
    guard let record = state.agents[agentID] else { return false }
    _ = try await manager.delete(sandboxID: record.sandboxID)
    state.agents.removeValue(forKey: agentID)
    let disposableSnapshots = state.snapshots.values.filter {
      $0.agentID == agentID && $0.publishedTemplateID == nil
    }
    for snapshot in disposableSnapshots {
      _ = try? manager.deleteSnapshot(snapshotID: snapshot.snapshotID)
      if let volumeSnapshot = snapshot.volumeSnapshotName {
        try? manager.deleteNamedVolume(volumeSnapshot)
      }
      state.snapshots.removeValue(forKey: snapshot.snapshotID)
    }
    state.operations = state.operations.filter { $0.value.agentID != agentID }
    if let volume = record.volumeName { try? manager.deleteNamedVolume(volume) }
    persist()
    return true
  }

  func pause(agentID: String) async throws -> AgentInstanceResponse? {
    guard var record = state.agents[agentID] else { return nil }
    guard try await manager.pause(sandboxID: record.sandboxID) else { return nil }
    record.status = "stopped"
    state.agents[agentID] = record
    persist()
    return response(for: record)
  }

  func resume(agentID: String) async throws -> AgentInstanceResponse? {
    guard var record = state.agents[agentID] else { return nil }
    guard try await manager.resume(sandboxID: record.sandboxID, timeout: Self.defaultTimeout) != nil else {
      return nil
    }
    record.status = "running"
    state.agents[agentID] = record
    persist()
    return response(for: record)
  }

  func restart(agentID: String) async throws -> AgentSetupResult {
    guard var record = state.agents[agentID] else { throw notFound(agentID) }
    let operationID = beginOperation(agentID: agentID, type: "restart")
    do {
      let result = try await configureRuntime(for: record, restart: true)
      record.setup = result
      record.status = result.exitCode == 0 ? "running" : "error"
      state.agents[agentID] = record
      finishOperation(operationID, status: result.exitCode == 0 ? "succeeded" : "failed", error: result.stderr)
      persist()
      return result
    } catch {
      record.status = "error"
      state.agents[agentID] = record
      finishOperation(operationID, status: "failed", error: error.localizedDescription)
      persist()
      throw error
    }
  }

  func upgrade(agentID: String) async throws -> AgentSetupResult {
    guard let record = state.agents[agentID] else { throw notFound(agentID) }
    let operationID = beginOperation(agentID: agentID, type: "upgrade")
    let result = try await manager.executeGuestShell(
      sandboxID: record.sandboxID,
      script: """
        set -eu
        if command -v openclaw >/dev/null 2>&1; then
          if openclaw --help 2>/dev/null | grep -q 'update'; then
            openclaw update
          else
            echo 'OpenClaw has no self-update command; runtime left unchanged'
          fi
        else
          echo 'OpenClaw executable is not installed in this template' >&2
          exit 127
        fi
        """,
      timeoutSeconds: 300
    )
    let setup = AgentSetupResult(exitCode: result.exitCode, stdout: result.stdout, stderr: result.stderr)
    finishOperation(operationID, status: result.exitCode == 0 ? "succeeded" : "failed", error: result.stderr)
    persist()
    return setup
  }

  func updateModel(agentID: String, request: UpdateAgentModelRequest) async throws -> AgentInstanceResponse {
    let model = request.model.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !model.isEmpty else { throw CubeVZError.invalidArguments("model is required") }
    guard var record = state.agents[agentID] else { throw notFound(agentID) }
    record.model = model
    let setup = try await configureRuntime(for: record, restart: true)
    record.setup = setup
    record.status = setup.exitCode == 0 ? "running" : "error"
    state.agents[agentID] = record
    persist()
    return response(for: record)
  }

  func updateWeCom(agentID: String, request: UpdateWeComConfigRequest) async throws -> AgentInstanceResponse {
    let botID = request.botId.trimmingCharacters(in: .whitespacesAndNewlines)
    let botSecret = request.botSecret.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !botID.isEmpty, !botSecret.isEmpty else {
      throw CubeVZError.invalidArguments("Bot ID and Secret must be provided together")
    }
    guard var record = state.agents[agentID] else { throw notFound(agentID) }
    record.wecomConfig = AgentWeComConfig(botId: botID, botSecret: botSecret)
    let setup = try await configureRuntime(for: record, restart: true)
    record.setup = setup
    record.status = setup.exitCode == 0 ? "running" : "error"
    state.agents[agentID] = record
    persist()
    return response(for: record)
  }

  func wecomConfig(agentID: String) throws -> AgentWeComConfig? {
    guard let record = state.agents[agentID] else { throw notFound(agentID) }
    return record.wecomConfig
  }

  func gatewayHealth(agentID: String) async throws -> AgentGatewayHealthResponse {
    guard let record = state.agents[agentID] else { throw notFound(agentID) }
    return AgentGatewayHealthResponse(
      ready: await manager.probeGuestHTTP(sandboxID: record.sandboxID, port: Self.openClawPort)
    )
  }

  func listOperations(agentID: String) throws -> [AgentOperationResponse] {
    guard state.agents[agentID] != nil else { throw notFound(agentID) }
    return state.operations.values
      .filter { $0.agentID == agentID }
      .sorted { $0.createdAt > $1.createdAt }
      .map(operationResponse(for:))
  }

  private func snapshotRecord(
    agent: AgentRecord,
    snapshot: SnapshotInfoResponse,
    parentSnapshotID: String? = nil
  ) throws -> SnapshotRecord {
    let volumeSnapshotName: String?
    if let sourceVolume = agent.volumeName {
      let destination = "agenthub-state-\(snapshot.snapshotID)"
      try manager.cloneNamedVolume(sourceName: sourceVolume, destinationName: destination)
      volumeSnapshotName = destination
    } else {
      volumeSnapshotName = nil
    }
    let now = Date()
    return SnapshotRecord(
      snapshotID: snapshot.snapshotID,
      agentID: agent.id,
      names: snapshot.names,
      status: "ready",
      isHealthy: true,
      publishedTemplateID: nil,
      model: agent.model,
      wecomConfig: agent.wecomConfig,
      volumeSnapshotName: volumeSnapshotName,
      sharedFilesTemplateID: agent.persistenceMode == "shared_files" ? agent.rootfsSourceID : nil,
      parentSnapshotID: parentSnapshotID,
      createdAt: now,
      updatedAt: now
    )
  }

  func createSnapshot(agentID: String, request: CreateAgentSnapshotRequest) async throws -> AgentSnapshotJobResponse {
    guard let record = state.agents[agentID] else { throw notFound(agentID) }
    let operationID = beginOperation(agentID: agentID, type: "snapshot")
    do {
      guard let snapshot = try await manager.createSnapshot(sandboxID: record.sandboxID, name: request.name) else {
        throw CubeVZError.runtime("sandbox not found: \(record.sandboxID)")
      }
      do {
        state.snapshots[snapshot.snapshotID] = try snapshotRecord(agent: record, snapshot: snapshot)
      } catch {
        _ = try? manager.deleteSnapshot(snapshotID: snapshot.snapshotID)
        throw error
      }
      finishOperation(operationID, status: "succeeded", targetID: snapshot.snapshotID)
      persist()
      return AgentSnapshotJobResponse(operationId: operationID, status: "succeeded")
    } catch {
      finishOperation(operationID, status: "failed", error: error.localizedDescription)
      persist()
      throw error
    }
  }

  func listSnapshots(agentID: String) throws -> [AgentSnapshotResponse] {
    guard state.agents[agentID] != nil else { throw notFound(agentID) }
    return state.snapshots.values
      .filter { $0.agentID == agentID }
      .sorted { $0.createdAt > $1.createdAt }
      .map(snapshotResponse(for:))
  }

  func deleteSnapshot(agentID: String, snapshotID: String) throws -> Bool {
    guard let snapshot = state.snapshots[snapshotID], snapshot.agentID == agentID else { return false }
    if snapshot.publishedTemplateID != nil {
      throw CubeVZError.runtime("snapshot is in use by a published AgentHub template")
    }
    guard try manager.deleteSnapshot(snapshotID: snapshotID) else { return false }
    if let volumeSnapshot = snapshot.volumeSnapshotName {
      try? manager.deleteNamedVolume(volumeSnapshot)
    }
    state.snapshots.removeValue(forKey: snapshotID)
    persist()
    return true
  }

  func updateSnapshot(
    agentID: String,
    snapshotID: String,
    request: UpdateAgentSnapshotRequest
  ) throws -> Bool {
    guard var snapshot = state.snapshots[snapshotID], snapshot.agentID == agentID else { return false }
    if let name = request.name?.trimmingCharacters(in: .whitespacesAndNewlines), !name.isEmpty {
      snapshot.names = [name]
    }
    if let isHealthy = request.isHealthy { snapshot.isHealthy = isHealthy }
    snapshot.updatedAt = Date()
    state.snapshots[snapshotID] = snapshot
    persist()
    return true
  }

  func rollback(agentID: String, request: RollbackAgentRequest) async throws -> AgentRollbackResponse {
    guard var record = state.agents[agentID] else { throw notFound(agentID) }
    guard let snapshot = state.snapshots[request.snapshotId], snapshot.agentID == agentID else {
      throw CubeVZError.runtime("snapshot not found: \(request.snapshotId)")
    }
    let operationID = beginOperation(agentID: agentID, type: "rollback")
    do {
      guard let response = try await manager.rollback(
        sandboxID: record.sandboxID,
        snapshotID: snapshot.snapshotID
      ) else { throw CubeVZError.runtime("sandbox not found: \(record.sandboxID)") }
      if let sourceVolume = snapshot.volumeSnapshotName, let destinationVolume = record.volumeName {
        guard try await manager.pause(sandboxID: record.sandboxID) else {
          throw CubeVZError.runtime("sandbox disappeared while restoring shared-files state")
        }
        do {
          try manager.restoreNamedVolume(snapshotName: sourceVolume, destinationName: destinationVolume)
          guard try await manager.resume(sandboxID: record.sandboxID, timeout: Self.defaultTimeout) != nil else {
            throw CubeVZError.runtime("sandbox disappeared while resuming after shared-files restore")
          }
        } catch {
          _ = try? await manager.resume(sandboxID: record.sandboxID, timeout: Self.defaultTimeout)
          throw error
        }
      }
      if let model = snapshot.model { record.model = model }
      record.wecomConfig = snapshot.wecomConfig
      state.agents[agentID] = record
      finishOperation(operationID, status: "succeeded", targetID: snapshot.snapshotID)
      persist()
      return AgentRollbackResponse(
        sandboxID: response.sandboxID,
        snapshotID: snapshot.snapshotID,
        operationID: operationID,
        status: "success"
      )
    } catch {
      finishOperation(operationID, status: "failed", targetID: snapshot.snapshotID, error: error.localizedDescription)
      persist()
      throw error
    }
  }

  func recover(agentID: String) async throws -> AgentRecoverResponse {
    guard state.agents[agentID] != nil else { throw notFound(agentID) }
    if let snapshot = state.snapshots.values
      .filter({ $0.agentID == agentID && $0.isHealthy })
      .sorted(by: { $0.updatedAt > $1.updatedAt })
      .first
    {
      _ = try await rollback(agentID: agentID, request: RollbackAgentRequest(snapshotId: snapshot.snapshotID))
      return AgentRecoverResponse(recovered: true, method: "rollback", snapshotID: snapshot.snapshotID)
    }
    _ = try await restart(agentID: agentID)
    return AgentRecoverResponse(recovered: true, method: "restart", snapshotID: nil)
  }

  func clone(agentID: String, request: CloneAgentRequest) async throws -> AgentInstanceResponse {
    guard let source = state.agents[agentID] else { throw notFound(agentID) }
    let requestedSnapshot = request.snapshotId?.trimmingCharacters(in: .whitespacesAndNewlines)
    let sourceSnapshot: SnapshotRecord
    if let requestedSnapshot, !requestedSnapshot.isEmpty {
      guard let snapshot = state.snapshots[requestedSnapshot], snapshot.agentID == agentID else {
        throw CubeVZError.runtime("snapshot not found: \(requestedSnapshot)")
      }
      sourceSnapshot = snapshot
    } else {
      guard let snapshot = try await manager.createSnapshot(
        sandboxID: source.sandboxID,
        name: "clone-source-\(source.name)"
      ) else { throw CubeVZError.runtime("sandbox not found: \(source.sandboxID)") }
      do {
        state.snapshots[snapshot.snapshotID] = try snapshotRecord(agent: source, snapshot: snapshot)
      } catch {
        _ = try? manager.deleteSnapshot(snapshotID: snapshot.snapshotID)
        throw error
      }
      guard let recorded = state.snapshots[snapshot.snapshotID] else {
        throw CubeVZError.runtime("AgentHub snapshot state is missing: \(snapshot.snapshotID)")
      }
      sourceSnapshot = recorded
    }
    let clone = CreateAgentInstanceRequest(
      name: request.name?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
        ? request.name! : "\(source.name) clone",
      engine: source.engine,
      model: sourceSnapshot.model ?? source.model,
      templateId: nil,
      snapshotId: sourceSnapshot.snapshotID,
      persistenceMode: source.persistenceMode,
      botId: sourceSnapshot.wecomConfig?.botId,
      botSecret: sourceSnapshot.wecomConfig?.botSecret
    )
    let result = try await createInstanceImpl(clone)
    persist()
    return result
  }

  func publishTemplate(agentID: String, request: PublishAgentTemplateRequest) async throws -> AgentPublishTemplateResponse {
    guard let agent = state.agents[agentID] else { throw notFound(agentID) }
    let snapshotID: String
    if let requested = request.snapshotId?.trimmingCharacters(in: .whitespacesAndNewlines), !requested.isEmpty {
      guard let snapshot = state.snapshots[requested], snapshot.agentID == agentID else {
        throw CubeVZError.runtime("snapshot not found: \(requested)")
      }
      snapshotID = snapshot.snapshotID
    } else {
      guard let snapshot = try await manager.createSnapshot(sandboxID: agent.sandboxID, name: request.name) else {
        throw CubeVZError.runtime("sandbox not found: \(agent.sandboxID)")
      }
      do {
        state.snapshots[snapshot.snapshotID] = try snapshotRecord(agent: agent, snapshot: snapshot)
      } catch {
        _ = try? manager.deleteSnapshot(snapshotID: snapshot.snapshotID)
        throw error
      }
      snapshotID = snapshot.snapshotID
    }
    guard let sourceSnapshot = state.snapshots[snapshotID] else {
      throw CubeVZError.runtime("AgentHub snapshot state is missing: \(snapshotID)")
    }
    let templateID = "agenttpl-\(UUID().uuidString.lowercased())"
    let now = Date()
    state.templates[templateID] = TemplateRecord(
      templateID: templateID,
      name: request.name?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
        ? request.name! : "\(agent.name) template",
      sourceAgentID: agentID,
      sourceSnapshotID: snapshotID,
      sourceSandboxID: agent.sandboxID,
      model: sourceSnapshot.model ?? agent.model,
      version: "cube-vz",
      persistenceMode: agent.persistenceMode,
      wecomConfig: sourceSnapshot.wecomConfig,
      volumeSnapshotName: sourceSnapshot.volumeSnapshotName,
      sharedFilesTemplateID: sourceSnapshot.sharedFilesTemplateID,
      recommended: false,
      createdAt: now
    )
    if var snapshot = state.snapshots[snapshotID] {
      snapshot.publishedTemplateID = templateID
      snapshot.updatedAt = now
      state.snapshots[snapshotID] = snapshot
    }
    persist()
    return AgentPublishTemplateResponse(templateId: templateID, snapshotId: snapshotID, name: request.name)
  }

  func listTemplates() -> [AgentTemplateResponse] {
    state.templates.values.sorted { $0.createdAt > $1.createdAt }.map(templateResponse(for:))
  }

  func registerMarketTemplate(_ request: RegisterMarketAgentTemplateRequest) throws -> AgentTemplateResponse {
    let templateID = request.templateId.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !templateID.isEmpty else { throw CubeVZError.invalidArguments("templateId is required") }
    guard manager.getTemplate(templateID: templateID) != nil else {
      throw CubeVZError.runtime("template not found: \(templateID)")
    }
    let now = Date()
    let record = TemplateRecord(
      templateID: templateID,
      name: nonEmpty(request.name) ?? templateID,
      sourceAgentID: "market",
      sourceSnapshotID: "",
      sourceSandboxID: "",
      model: nonEmpty(request.model) ?? state.settings.model,
      version: nonEmpty(request.version) ?? "market",
      persistenceMode: nil,
      wecomConfig: nil,
      volumeSnapshotName: nil,
      sharedFilesTemplateID: nil,
      recommended: request.recommended ?? false,
      createdAt: state.templates[templateID]?.createdAt ?? now
    )
    state.templates[templateID] = record
    persist()
    return templateResponse(for: record)
  }

  func updateTemplate(templateID: String, request: UpdateAgentTemplateRequest) throws -> Bool {
    guard var record = state.templates[templateID] else { return false }
    if let name = nonEmpty(request.name) { record.name = name }
    if let recommended = request.recommended { record.recommended = recommended }
    state.templates[templateID] = record
    persist()
    return true
  }

  func deleteTemplate(templateID: String) throws -> Bool {
    guard let record = state.templates[templateID] else { return false }
    guard !state.agents.values.contains(where: { $0.templateID == templateID }) else {
      throw CubeVZError.runtime("AgentHub template is in use")
    }
    state.templates.removeValue(forKey: templateID)
    if var snapshot = state.snapshots[record.sourceSnapshotID] {
      snapshot.publishedTemplateID = nil
      snapshot.updatedAt = Date()
      if state.agents[snapshot.agentID] == nil {
        _ = try? manager.deleteSnapshot(snapshotID: snapshot.snapshotID)
        if let volumeSnapshot = snapshot.volumeSnapshotName {
          try? manager.deleteNamedVolume(volumeSnapshot)
        }
        state.snapshots.removeValue(forKey: snapshot.snapshotID)
      } else {
        state.snapshots[record.sourceSnapshotID] = snapshot
      }
    }
    persist()
    return true
  }

  func settings() -> AgentSettingsResponse { settingsResponse() }

  func updateSettings(_ request: UpdateAgentSettingsRequest) async throws -> AgentSettingsResponse {
    var updated = state.settings
    var changed = false
    if let provider = nonEmpty(request.llmProvider) { updated.provider = provider; changed = true }
    if let baseURL = nonEmpty(request.llmBaseUrl) {
      guard let url = URL(string: baseURL), url.scheme == "http" || url.scheme == "https", url.host != nil else {
        throw CubeVZError.invalidArguments("llmBaseUrl must be an http(s) URL")
      }
      updated.baseURL = url.absoluteString.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
      changed = true
    }
    if let model = nonEmpty(request.llmModel) { updated.model = model; changed = true }
    if let apiKey = nonEmpty(request.llmApiKey) ?? nonEmpty(request.deepseekApiKey) {
      updated.apiKey = apiKey
      changed = true
    }
    if let mode = nonEmpty(request.llmCredentialMode) {
      guard mode == "egress" || mode == "env" else {
        throw CubeVZError.invalidArguments("llmCredentialMode must be egress or env")
      }
      updated.credentialMode = mode
      changed = true
    }
    if let domain = request.gatewayDomain {
      let normalized = domain.trimmingCharacters(in: .whitespacesAndNewlines)
      if !normalized.isEmpty,
        normalized.range(of: "^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$", options: .regularExpression) == nil
      {
        throw CubeVZError.invalidArguments("gatewayDomain is invalid")
      }
      updated.gatewayDomain = normalized.isEmpty ? nil : normalized.lowercased()
      changed = true
    }
    guard changed else { throw CubeVZError.invalidArguments("no settings provided to update") }
    state.settings = updated
    for (agentID, var record) in state.agents {
      if manager.get(sandboxID: record.sandboxID)?.state == "running" {
        _ = try? await manager.updateNetwork(
          sandboxID: record.sandboxID,
          allowInternetAccess: updated.credentialMode == "env",
          network: networkPolicy(for: updated)
        )
        let setup = try? await configureRuntime(for: record, restart: false)
        if let setup { record.setup = setup; state.agents[agentID] = record }
      }
    }
    persist()
    return settingsResponse()
  }

  private func createInstanceImpl(_ request: CreateAgentInstanceRequest) async throws -> AgentInstanceResponse {
    let resolved = try resolveSource(request)
    let mode = try persistenceMode(request.persistenceMode ?? resolved.persistenceMode)
    let agentID = "agent-\(UUID().uuidString.lowercased())"
    let volumeName = mode == "shared_files"
      ? "agenthub-openclaw-\(UUID().uuidString.lowercased())" : nil
    let wecom = try validateWeCom(botID: request.botId, botSecret: request.botSecret) ?? resolved.wecomConfig
    let model = nonEmpty(request.model) ?? resolved.templateModel ?? state.settings.model
    let metadata: [String: String] = [
      "agenthub": "true",
      "agenthub.id": agentID,
      "agenthub.name": request.name.trimmingCharacters(in: .whitespacesAndNewlines),
      "agenthub.engine": "openclaw",
      "agenthub.persistence_mode": mode,
      "agenthub.rootfs_source_type": resolved.kind,
      "agenthub.rootfs_source_id": resolved.sourceID,
    ]
    if let sourceVolume = resolved.volumeSnapshotName, let destinationVolume = volumeName {
      try manager.cloneNamedVolume(sourceName: sourceVolume, destinationName: destinationVolume)
    }
    let created: SandboxResponse
    do {
      created = try await manager.create(request: CreateSandboxRequest(
        templateID: resolved.sourceID,
        timeout: Self.defaultTimeout,
        allowInternetAccess: state.settings.credentialMode == "env",
        network: networkPolicy(for: state.settings),
        metadata: metadata,
        volumeMounts: volumeName.map { [SandboxVolumeMountRequest(name: $0, path: "/root/.openclaw")] }
      ))
    } catch {
      if let volumeName { try? manager.deleteNamedVolume(volumeName) }
      throw error
    }
    var record = AgentRecord(
      id: agentID,
      name: request.name.trimmingCharacters(in: .whitespacesAndNewlines),
      status: "starting",
      engine: "openclaw",
      model: model,
      sandboxID: created.sandboxID,
      templateID: resolved.displayTemplateID,
      persistenceMode: mode,
      rootfsSourceType: resolved.kind,
      rootfsSourceID: resolved.sourceID,
      volumeName: volumeName,
      gatewayToken: randomToken(),
      wecomConfig: wecom,
      setup: nil,
      createdAt: Date()
    )
    do {
      let setup = try await configureRuntime(for: record, restart: true)
      record.setup = setup
      record.status = setup.exitCode == 0 ? "running" : "error"
      state.agents[agentID] = record
      persist()
      return response(for: record)
    } catch {
      _ = try? await manager.delete(sandboxID: created.sandboxID)
      if let volumeName { try? manager.deleteNamedVolume(volumeName) }
      throw error
    }
  }

  private func configureRuntime(for record: AgentRecord, restart: Bool) async throws -> AgentSetupResult {
    let modelID = record.model.split(separator: "/", maxSplits: 1).last.map(String.init) ?? record.model
    let openClawPrimary = "\(state.settings.provider)/\(modelID)"
    let apiKey = state.settings.credentialMode == "egress"
      ? "CUBEVZ_EGRESS_MANAGED" : state.settings.apiKey ?? ""
    var config: [String: Any] = [
      "gateway": [
        "bind": "lan",
        "port": Int(Self.openClawPort),
        "mode": "local",
        "tailscale": ["mode": "off", "resetOnExit": false],
        "auth": ["mode": "token", "token": record.gatewayToken],
        "trustedProxies": ["127.0.0.1", "::1"],
        "controlUi": [
          "allowedOrigins": ["*"],
          "dangerouslyDisableDeviceAuth": true,
          "allowInsecureAuth": true,
          "dangerouslyAllowHostHeaderOriginFallback": true,
        ],
      ],
      "models": [
        "mode": "merge",
        "providers": [
          state.settings.provider: [
            "baseUrl": state.settings.baseURL,
            "api": "openai-completions",
            "models": [[
              "id": modelID,
              "name": modelID,
              "reasoning": true,
              "input": ["text"],
              "contextWindow": 1_000_000,
              "maxTokens": 384_000,
              "api": "openai-completions",
            ]],
          ]
        ],
      ],
      "agents": ["defaults": [
        "model": ["primary": openClawPrimary],
        "models": [openClawPrimary: ["alias": modelID]],
        "workspace": "/root/.openclaw/workspace",
      ]],
      "auth": ["profiles": [
        "\(state.settings.provider):default": [
          "provider": state.settings.provider,
          "mode": "api_key",
        ]
      ]],
      "session": ["dmScope": "per-channel-peer"],
      "tools": ["profile": "full"],
      "skills": ["install": ["nodeManager": "npm"]],
      "cubeVZ": [
        "agentID": record.id,
        "credentialMode": state.settings.credentialMode,
      ],
    ]
    if let wecom = record.wecomConfig {
      config["channels"] = ["wecom": ["botId": wecom.botId, "secret": wecom.botSecret]]
    }
    let data = try JSONSerialization.data(withJSONObject: config, options: [.prettyPrinted, .sortedKeys])
    let authProfiles: [String: Any] = [
      "version": 1,
      "profiles": [
        "\(state.settings.provider):default": [
          "type": "api_key",
          "provider": state.settings.provider,
          "key": apiKey,
        ]
      ],
    ]
    let authProfilesData = try JSONSerialization.data(
      withJSONObject: authProfiles,
      options: [.prettyPrinted, .sortedKeys]
    )
    if let volumeName = record.volumeName {
      // A restored VZ machine state can retain an old envd mount namespace.
      // The named volume is the shared-files persistence boundary itself, so
      // write configuration atomically there instead of relying on that stale
      // guest file RPC. The guest sees the same virtiofs content.
      try manager.writeNamedVolumeFile(
        volumeName: volumeName,
        relativePath: "openclaw.json",
        contents: data
      )
      try manager.writeNamedVolumeFile(
        volumeName: volumeName,
        relativePath: "agents/main/agent/auth-profiles.json",
        contents: authProfilesData
      )
    } else {
      let makeDirectory = try await manager.executeGuestShell(
        sandboxID: record.sandboxID,
        script: "mkdir -p /root/.openclaw/agents/main/agent /root/.openclaw/workspace /root/.openclaw/agents/main/sessions",
        timeoutSeconds: 10
      )
      guard makeDirectory.exitCode == 0 else {
        throw CubeVZError.runtime("cannot initialize OpenClaw state directory: \(makeDirectory.stderr)")
      }
      try await writeRuntimeFile(
        sandboxID: record.sandboxID,
        path: "/root/.openclaw/openclaw.json",
        contents: data
      )
      try await writeRuntimeFile(
        sandboxID: record.sandboxID,
        path: "/root/.openclaw/agents/main/agent/auth-profiles.json",
        contents: authProfilesData
      )
    }
    guard restart else {
      return AgentSetupResult(exitCode: 0, stdout: "OpenClaw configuration updated\n", stderr: "")
    }
    let command = try await manager.executeGuestShell(
      sandboxID: record.sandboxID,
      script: """
        set -eu
        if command -v supervisorctl >/dev/null 2>&1 && supervisorctl status openclaw >/dev/null 2>&1; then
          supervisorctl restart openclaw
          echo 'OpenClaw gateway restarted through supervisor'
        elif command -v openclaw >/dev/null 2>&1; then
          pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
          nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
          echo 'OpenClaw gateway restarted'
        elif [ -x /opt/openclaw/openclaw ]; then
          pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
          nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
          echo 'OpenClaw gateway restarted from /opt/openclaw'
        elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
          (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
          echo 'OpenClaw gateway restarted through npm'
        elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
          (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
          echo 'OpenClaw gateway restarted through npm'
        else
          echo 'OpenClaw executable is not installed; persisted configuration for template startup' >&2
        fi
        """,
      timeoutSeconds: 30
    )
    return AgentSetupResult(exitCode: command.exitCode, stdout: command.stdout, stderr: command.stderr)
  }

  /// Prefer envd's file endpoint, but a sandbox restored from a VM state can
  /// retain an old file-service mount namespace. The command data plane runs
  /// in the current guest mount namespace, so it is a safe bounded fallback.
  private func writeRuntimeFile(sandboxID: String, path: String, contents: Data) async throws {
    do {
      try await manager.writeGuestFile(sandboxID: sandboxID, path: path, contents: contents)
      return
    } catch {
      let encoded = contents.base64EncodedString()
      let result = try await manager.executeGuestShell(
        sandboxID: sandboxID,
        script: """
          set -eu
          printf '%s' '\(encoded)' | base64 -d > \(path)
          chmod 600 \(path)
          """,
        timeoutSeconds: 30
      )
      guard result.exitCode == 0 else {
        throw CubeVZError.runtime(
          "cannot write OpenClaw runtime file: \(result.stderr.isEmpty ? error.localizedDescription : result.stderr)"
        )
      }
    }
  }

  private func resolveSource(_ request: CreateAgentInstanceRequest) throws -> (
    sourceID: String,
    kind: String,
    displayTemplateID: String,
    templateModel: String?,
    volumeSnapshotName: String?,
    persistenceMode: String?,
    wecomConfig: AgentWeComConfig?
  ) {
    if let snapshot = nonEmpty(request.snapshotId) {
      if request.persistenceMode != "full_snapshot",
        let record = state.snapshots[snapshot],
        let source = record.sharedFilesTemplateID,
        manager.getTemplate(templateID: source) != nil
      {
        return (
          source,
          "snapshot",
          snapshot,
          record.model,
          record.volumeSnapshotName,
          "shared_files",
          record.wecomConfig
        )
      }
      guard manager.getTemplate(templateID: snapshot) != nil else { throw CubeVZError.runtime("template not found: \(snapshot)") }
      return (snapshot, "snapshot", snapshot, state.snapshots[snapshot]?.model, nil, "full_snapshot", state.snapshots[snapshot]?.wecomConfig)
    }
    let requested = nonEmpty(request.templateId) ?? defaultTemplateID
    if let template = state.templates[requested] {
      let source = template.sourceSnapshotID.isEmpty ? template.templateID : template.sourceSnapshotID
      if request.persistenceMode != "full_snapshot",
        let sharedFilesTemplateID = template.sharedFilesTemplateID,
        manager.getTemplate(templateID: sharedFilesTemplateID) != nil
      {
        return (
          sharedFilesTemplateID,
          "snapshot",
          template.templateID,
          template.model,
          template.volumeSnapshotName,
          "shared_files",
          template.wecomConfig
        )
      }
      guard manager.getTemplate(templateID: source) != nil else { throw CubeVZError.runtime("template not found: \(source)") }
      return (
        source,
        template.sourceSnapshotID.isEmpty ? "template" : "snapshot",
        template.templateID,
        template.model,
        nil,
        template.persistenceMode,
        template.wecomConfig
      )
    }
    guard manager.getTemplate(templateID: requested) != nil else {
      throw CubeVZError.runtime("template not found: \(requested)")
    }
    return (requested, "template", requested, nil, nil, nil, nil)
  }

  private func validate(_ request: CreateAgentInstanceRequest) throws {
    guard !request.name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
      throw CubeVZError.invalidArguments("agent name is required")
    }
    guard request.engine == "openclaw" else {
      throw CubeVZError.invalidArguments("only openclaw engine is currently supported")
    }
    _ = try validateWeCom(botID: request.botId, botSecret: request.botSecret)
    _ = try persistenceMode(request.persistenceMode)
  }

  private func validateWeCom(botID: String?, botSecret: String?) throws -> AgentWeComConfig? {
    let id = nonEmpty(botID)
    let secret = nonEmpty(botSecret)
    guard (id == nil) == (secret == nil) else {
      throw CubeVZError.invalidArguments("Bot ID and Secret must be provided together")
    }
    return id.flatMap { botID in secret.map { AgentWeComConfig(botId: botID, botSecret: $0) } }
  }

  private func persistenceMode(_ raw: String?) throws -> String {
    let value = nonEmpty(raw) ?? "full_snapshot"
    guard value == "full_snapshot" || value == "shared_files" else {
      throw CubeVZError.invalidArguments("persistenceMode must be full_snapshot or shared_files")
    }
    return value
  }

  private func networkPolicy(for settings: Settings) -> SandboxNetworkRequest? {
    guard settings.credentialMode == "egress", let apiKey = nonEmpty(settings.apiKey),
      let host = URL(string: settings.baseURL)?.host
    else { return nil }
    return SandboxNetworkRequest(
      allowPublicTraffic: nil,
      allowOut: [host],
      denyOut: nil,
      maskRequestHost: nil,
      rules: [
        EgressRuleRequest(
          name: "agenthub-llm",
          match: EgressRuleMatchRequest(sni: host, host: host, method: nil, path: nil, scheme: nil),
          action: EgressRuleActionRequest(
            allow: true,
            audit: "metadata",
            inject: [EgressRuleInjectRequest(header: "Authorization", secret: apiKey, format: "Bearer ${SECRET}")]
          )
        )
      ]
    )
  }

  private func response(for record: AgentRecord) -> AgentInstanceResponse {
    let liveStatus: String
    if let info = manager.get(sandboxID: record.sandboxID) {
      liveStatus = info.state == "paused" ? "stopped" : record.status
    } else {
      liveStatus = "error"
    }
    let domain = state.settings.gatewayDomain ?? "cube.local"
    let gateway = "http://\(Self.openClawPort)-\(record.sandboxID).\(domain)#token=\(record.gatewayToken)"
    let bots = record.wecomConfig == nil ? [] : ["wecom"]
    return AgentInstanceResponse(
      id: record.id,
      name: record.name,
      status: liveStatus,
      engine: record.engine,
      env: "linux",
      model: record.model,
      version: "cube-vz",
      bots: bots,
      botsAvailable: record.wecomConfig == nil ? ["wecom"] : [],
      avatar: record.name,
      avatarTone: "sky",
      sandboxId: record.sandboxID,
      templateId: record.templateID,
      gatewayUrl: gateway,
      envUrl: "http://49983-\(record.sandboxID).\(domain)",
      persistenceMode: record.persistenceMode,
      rootfsSourceType: record.rootfsSourceType,
      rootfsSourceId: record.rootfsSourceID,
      openclawPersistId: record.volumeName,
      openclawStatePath: record.volumeName.map { _ in "/root/.openclaw" },
      wecomConfig: record.wecomConfig,
      setup: record.setup
    )
  }

  private func snapshotResponse(for record: SnapshotRecord) -> AgentSnapshotResponse {
    let agent = state.agents[record.agentID]
    return AgentSnapshotResponse(
      snapshotID: record.snapshotID,
      names: record.names,
      status: record.status,
      snapshotKind: "sandbox",
      originSandboxID: agent?.sandboxID,
      publishedTemplateId: record.publishedTemplateID,
      rootfsSourceType: "snapshot",
      rootfsSourceId: record.snapshotID,
      rootfsSnapshotId: record.snapshotID,
      openclawStateSnapshotPath: record.volumeSnapshotName == nil ? nil : "/root/.openclaw",
      templateReferenced: record.publishedTemplateID != nil,
      isHealthy: record.isHealthy,
      parentSnapshotID: record.parentSnapshotID,
      createdAt: Self.dateString(record.createdAt),
      updatedAt: Self.dateString(record.updatedAt)
    )
  }

  private func templateResponse(for record: TemplateRecord) -> AgentTemplateResponse {
    AgentTemplateResponse(
      templateId: record.templateID,
      name: record.name,
      sourceAgentId: record.sourceAgentID,
      sourceSnapshotId: record.sourceSnapshotID,
      sourceSandboxId: record.sourceSandboxID,
      model: record.model,
      version: record.version,
      persistenceMode: record.persistenceMode,
      recommended: record.recommended,
      createdAt: Self.dateString(record.createdAt)
    )
  }

  private func operationResponse(for record: OperationRecord) -> AgentOperationResponse {
    AgentOperationResponse(
      operationId: record.operationID,
      agentId: record.agentID,
      operationType: record.operationType,
      status: record.status,
      targetId: record.targetID,
      errorMessage: record.errorMessage,
      createdAt: Self.dateString(record.createdAt),
      updatedAt: Self.dateString(record.updatedAt)
    )
  }

  private func settingsResponse() -> AgentSettingsResponse {
    let configured = nonEmpty(state.settings.apiKey) != nil
    let masked = state.settings.apiKey.map(Self.masked)
    return AgentSettingsResponse(
      deepseekApiKeyConfigured: configured,
      deepseekApiKeyMasked: masked,
      source: configured ? "database" : "none",
      llmProvider: state.settings.provider,
      llmBaseUrl: state.settings.baseURL,
      llmModel: state.settings.model,
      llmApiKeyConfigured: configured,
      llmApiKeyMasked: masked,
      llmApiKeySource: configured ? "database" : "none",
      llmCredentialMode: state.settings.credentialMode,
      persistenceEnabled: true,
      gatewayDomain: state.settings.gatewayDomain
    )
  }

  private func beginOperation(agentID: String, type: String) -> String {
    let id = "op-\(UUID().uuidString.lowercased())"
    let now = Date()
    state.operations[id] = OperationRecord(
      operationID: id,
      agentID: agentID,
      operationType: type,
      status: "running",
      targetID: nil,
      errorMessage: nil,
      createdAt: now,
      updatedAt: now
    )
    persist()
    return id
  }

  private func finishOperation(
    _ operationID: String,
    status: String,
    targetID: String? = nil,
    error: String? = nil
  ) {
    guard var record = state.operations[operationID] else { return }
    record.status = status
    record.targetID = targetID
    record.errorMessage = error?.isEmpty == false ? error : nil
    record.updatedAt = Date()
    state.operations[operationID] = record
  }

  private func persist() {
    do {
      let encoder = JSONEncoder()
      encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
      try encoder.encode(state).write(to: stateURL, options: .atomic)
      try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: stateURL.path)
    } catch {
      FileHandle.standardError.write(Data("cube-vz-api: cannot persist AgentHub state: \(error)\n".utf8))
    }
  }

  private func notFound(_ agentID: String) -> CubeVZError {
    .runtime("AgentHub instance not found: \(agentID)")
  }

  private static func dateString(_ date: Date) -> String {
    ISO8601DateFormatter().string(from: date)
  }

  private static func masked(_ value: String) -> String {
    guard value.count > 8 else { return "••••" }
    return "\(value.prefix(4))••••\(value.suffix(4))"
  }

  private func nonEmpty(_ value: String?) -> String? {
    guard let value else { return nil }
    let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
    return trimmed.isEmpty ? nil : trimmed
  }

  private func randomToken() -> String {
    UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
  }
}
