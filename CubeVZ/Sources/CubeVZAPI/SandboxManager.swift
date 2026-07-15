// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Foundation
@preconcurrency import Virtualization

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
    let reusableMACAddress: String
  }

  private let templateID: String
  private let templateDirectory: VMDirectory
  private let sandboxesDirectory: URL
  private var sandboxes: [String: RunningSandbox] = [:]
  private var reusableMACAddresses: [String] = []

  init(templateID: String, templateDirectory: URL, sandboxesDirectory: URL) throws {
    self.templateID = templateID
    self.templateDirectory = VMDirectory(url: templateDirectory)
    self.sandboxesDirectory = sandboxesDirectory

    let manifest = try self.templateDirectory.loadManifest()
    try self.templateDirectory.validateFiles(for: manifest)
    try FileManager.default.createDirectory(
      at: sandboxesDirectory,
      withIntermediateDirectories: true
    )
  }

  func create(templateID requestedTemplateID: String) async throws -> SandboxResponse {
    guard requestedTemplateID == templateID else {
      throw CubeVZError.invalidArguments("unknown templateID: \(requestedTemplateID)")
    }

    let clock = ContinuousClock()
    let startedAt = clock.now

    let sandboxID = "sb-\(UUID().uuidString.lowercased())"
    let destination = sandboxesDirectory.appendingPathComponent(
      sandboxID,
      isDirectory: true
    )
    let reusableMACAddress =
      reusableMACAddresses.popLast()
      ?? VZMACAddress.randomLocallyAdministered().string
    let directory = try VMTemplateCloner.cloneCold(
      template: templateDirectory,
      to: destination,
      macAddress: reusableMACAddress
    )
    let clonedAt = clock.now
    var virtualMachine: ManagedVM?

    do {
      let manifest = try directory.loadManifest()
      let createdVirtualMachine = try ManagedVM(directory: directory, manifest: manifest)
      let constructedAt = clock.now
      virtualMachine = createdVirtualMachine
      try await createdVirtualMachine.start()
      let startedVirtualMachineAt = clock.now
      try await createdVirtualMachine.waitUntilReady(timeout: .seconds(10))
      let readyAt = clock.now
      sandboxes[sandboxID] = RunningSandbox(
        directory: directory,
        virtualMachine: createdVirtualMachine,
        reusableMACAddress: reusableMACAddress
      )
      Self.logCreateTiming(
        sandboxID: sandboxID,
        clone: startedAt.duration(to: clonedAt),
        construct: clonedAt.duration(to: constructedAt),
        start: constructedAt.duration(to: startedVirtualMachineAt),
        readiness: startedVirtualMachineAt.duration(to: readyAt),
        guestReadiness: createdVirtualMachine.readinessMetadata,
        total: startedAt.duration(to: readyAt)
      )
      return SandboxResponse(templateID: templateID, sandboxID: sandboxID)
    } catch {
      if let virtualMachine {
        try? await virtualMachine.shutdown()
      }
      try? FileManager.default.removeItem(at: destination)
      reusableMACAddresses.append(reusableMACAddress)
      throw error
    }
  }

  func delete(sandboxID: String) async throws -> Bool {
    guard let sandbox = sandboxes[sandboxID] else {
      return false
    }
    // Sandbox disks are ephemeral and removed immediately, so waiting for a
    // guest userspace shutdown cannot preserve any state useful to DELETE.
    try await sandbox.virtualMachine.discard()
    try FileManager.default.removeItem(at: sandbox.directory.url)
    sandboxes.removeValue(forKey: sandboxID)
    reusableMACAddresses.append(sandbox.reusableMACAddress)
    return true
  }

  func openDataPlane(sandboxID: String, guestPort: UInt32) async throws -> VMStreamConnection {
    guard guestPort == 49_983 else {
      throw CubeVZError.invalidArguments("unsupported guest port: \(guestPort)")
    }
    guard let sandbox = sandboxes[sandboxID] else {
      throw CubeVZError.runtime("sandbox not found: \(sandboxID)")
    }

    var lastError: Error?
    for _ in 0..<20 {
      do {
        return try await sandbox.virtualMachine.connect(toGuestPort: guestPort)
      } catch {
        lastError = error
        try? await Task.sleep(for: .milliseconds(25))
      }
    }
    throw lastError ?? CubeVZError.runtime("cannot connect to guest port \(guestPort)")
  }

  private static func logCreateTiming(
    sandboxID: String,
    clone: Duration,
    construct: Duration,
    start: Duration,
    readiness: Duration,
    guestReadiness: String?,
    total: Duration
  ) {
    let message = String(
      format:
        "cube-vz-api: create sandbox=%@ mode=cold clone_ms=%.3f construct_ms=%.3f start_ms=%.3f ready_ms=%.3f total_ms=%.3f guest=%@\n",
      sandboxID,
      milliseconds(clone),
      milliseconds(construct),
      milliseconds(start),
      milliseconds(readiness),
      milliseconds(total),
      (guestReadiness ?? "none").replacingOccurrences(of: " ", with: ",")
    )
    FileHandle.standardError.write(Data(message.utf8))
  }

  private static func milliseconds(_ duration: Duration) -> Double {
    let components = duration.components
    return Double(components.seconds) * 1_000
      + Double(components.attoseconds) / 1_000_000_000_000_000
  }
}
