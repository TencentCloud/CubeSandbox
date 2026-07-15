// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import CubeVZCore
import Darwin
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
  private let sandboxesLockDescriptor: Int32
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
    let lockURL = sandboxesDirectory.appendingPathComponent(".cube-vz.lock")
    let lockDescriptor = Darwin.open(lockURL.path, O_CREAT | O_RDWR, S_IRUSR | S_IWUSR)
    guard lockDescriptor >= 0 else {
      throw CubeVZError.filesystem(
        "cannot open sandbox directory lock: \(String(cString: strerror(errno)))"
      )
    }
    guard Self.setDirectoryLock(lockDescriptor, type: Int16(F_WRLCK)) else {
      Darwin.close(lockDescriptor)
      throw CubeVZError.runtime(
        "sandboxes directory is already in use: \(sandboxesDirectory.path)"
      )
    }
    do {
      try Self.removeStaleSandboxes(in: sandboxesDirectory)
    } catch {
      _ = Self.setDirectoryLock(lockDescriptor, type: Int16(F_UNLCK))
      Darwin.close(lockDescriptor)
      throw error
    }
    sandboxesLockDescriptor = lockDescriptor
  }

  deinit {
    _ = Self.setDirectoryLock(sandboxesLockDescriptor, type: Int16(F_UNLCK))
    Darwin.close(sandboxesLockDescriptor)
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

  private static func removeStaleSandboxes(in directory: URL) throws {
    let entries: [URL]
    do {
      entries = try FileManager.default.contentsOfDirectory(
        at: directory,
        includingPropertiesForKeys: nil
      )
    } catch {
      throw CubeVZError.filesystem(
        "cannot inspect sandboxes directory \(directory.path): \(error)"
      )
    }

    for entry in entries {
      let name = entry.lastPathComponent
      let isSandbox = name.hasPrefix("sb-")
      let isPartialSandbox = name.hasPrefix(".sb-") && name.contains(".partial-")
      guard isSandbox || isPartialSandbox else { continue }
      do {
        try FileManager.default.removeItem(at: entry)
      } catch {
        throw CubeVZError.filesystem("cannot remove stale sandbox \(entry.path): \(error)")
      }
    }
  }

  nonisolated private static func setDirectoryLock(_ descriptor: Int32, type: Int16) -> Bool {
    var lock = flock()
    lock.l_start = 0
    lock.l_len = 0
    lock.l_pid = 0
    lock.l_type = type
    lock.l_whence = Int16(SEEK_SET)
    return Darwin.fcntl(descriptor, F_SETLK, &lock) == 0
  }

  private static func milliseconds(_ duration: Duration) -> Double {
    let components = duration.components
    return Double(components.seconds) * 1_000
      + Double(components.attoseconds) / 1_000_000_000_000_000
  }
}
