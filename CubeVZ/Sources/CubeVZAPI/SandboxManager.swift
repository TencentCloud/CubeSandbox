// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation

struct CreateSandboxRequest: Decodable {
  let templateID: String
}

struct SandboxResponse: Encodable {
  let templateID: String
  let sandboxID: String
  let clientID = "cube-vz-local"
  let envdVersion = "cube-vz"
}

@MainActor
final class SandboxManager {
  private struct RunningSandbox {
    let directory: VMDirectory
    let virtualMachine: ManagedVM
  }

  private let templateID: String
  private let templateDirectory: VMDirectory
  private let sandboxesDirectory: URL
  private var sandboxes: [String: RunningSandbox] = [:]

  init(templateID: String, templateDirectory: URL, sandboxesDirectory: URL) throws {
    self.templateID = templateID
    self.templateDirectory = VMDirectory(url: templateDirectory)
    self.sandboxesDirectory = sandboxesDirectory

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
  }

  func create(templateID requestedTemplateID: String) async throws -> SandboxResponse {
    guard requestedTemplateID == templateID else {
      throw CubeVZError.invalidArguments("unknown templateID: \(requestedTemplateID)")
    }

    let sandboxID = "sb-\(UUID().uuidString.lowercased())"
    let destination = sandboxesDirectory.appendingPathComponent(
      sandboxID,
      isDirectory: true
    )
    let directory = try VMTemplateCloner.clone(
      template: templateDirectory,
      to: destination
    )
    var virtualMachine: ManagedVM?

    do {
      let manifest = try directory.loadManifest()
      let createdVirtualMachine = try ManagedVM(directory: directory, manifest: manifest)
      virtualMachine = createdVirtualMachine
      _ = try await createdVirtualMachine.start()
      try await createdVirtualMachine.waitUntilReady(timeout: .seconds(10))
      sandboxes[sandboxID] = RunningSandbox(
        directory: directory,
        virtualMachine: createdVirtualMachine
      )
      return SandboxResponse(templateID: templateID, sandboxID: sandboxID)
    } catch {
      if let virtualMachine {
        try? await virtualMachine.shutdown()
      }
      try? FileManager.default.removeItem(at: destination)
      throw error
    }
  }

  func delete(sandboxID: String) async throws -> Bool {
    guard let sandbox = sandboxes[sandboxID] else {
      return false
    }
    try await sandbox.virtualMachine.shutdown()
    try FileManager.default.removeItem(at: sandbox.directory.url)
    sandboxes.removeValue(forKey: sandboxID)
    return true
  }
}
