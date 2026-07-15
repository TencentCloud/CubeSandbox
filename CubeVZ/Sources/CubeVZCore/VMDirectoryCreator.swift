// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation
@preconcurrency import Virtualization

public struct CreateVMRequest: Sendable {
  public var destination: URL
  public var kernel: URL
  public var disk: URL
  public var initrd: URL?
  public var cpuCount: Int
  public var memoryMiB: UInt64
  public var commandLine: String
  public var networkEnabled: Bool
  public var vsockEnabled: Bool
  public var allowFullCopy: Bool

  public init(
    destination: URL,
    kernel: URL,
    disk: URL,
    initrd: URL? = nil,
    cpuCount: Int = 2,
    memoryMiB: UInt64 = 2_048,
    commandLine: String = "console=hvc0 root=/dev/vda rw",
    networkEnabled: Bool = true,
    vsockEnabled: Bool = true,
    allowFullCopy: Bool = false
  ) {
    self.destination = destination
    self.kernel = kernel
    self.disk = disk
    self.initrd = initrd
    self.cpuCount = cpuCount
    self.memoryMiB = memoryMiB
    self.commandLine = commandLine
    self.networkEnabled = networkEnabled
    self.vsockEnabled = vsockEnabled
    self.allowFullCopy = allowFullCopy
  }
}

public enum VMDirectoryCreator {
  public static func create(_ request: CreateVMRequest) throws -> VMDirectory {
    let destination = request.destination.standardizedFileURL
    let manager = FileManager.default
    guard !manager.fileExists(atPath: destination.path) else {
      throw CubeVZError.filesystem("destination already exists: \(destination.path)")
    }

    let parent = destination.deletingLastPathComponent()
    let temporary = parent.appendingPathComponent(
      ".\(destination.lastPathComponent).partial-\(UUID().uuidString)"
    )
    do {
      try manager.createDirectory(at: temporary, withIntermediateDirectories: true)
      let vmDirectory = VMDirectory(url: temporary)

      try FileCloner.clone(
        from: request.kernel,
        to: vmDirectory.fileURL(named: VMDirectory.kernelFilename),
        allowFullCopy: request.allowFullCopy
      )
      try FileCloner.clone(
        from: request.disk,
        to: vmDirectory.fileURL(named: VMDirectory.diskFilename),
        allowFullCopy: request.allowFullCopy
      )

      var initrdFilename: String?
      if let initrd = request.initrd {
        try FileCloner.clone(
          from: initrd,
          to: vmDirectory.fileURL(named: VMDirectory.initrdFilename),
          allowFullCopy: request.allowFullCopy
        )
        initrdFilename = VMDirectory.initrdFilename
      }

      let machineIdentifier = VZGenericMachineIdentifier()
      try machineIdentifier.dataRepresentation.write(
        to: vmDirectory.machineIdentifierURL,
        options: .atomic
      )

      let manifest = VMManifest(
        cpuCount: request.cpuCount,
        memoryMiB: request.memoryMiB,
        commandLine: request.commandLine,
        initrdFile: initrdFilename,
        networkEnabled: request.networkEnabled,
        vsockEnabled: request.vsockEnabled,
        macAddress: request.networkEnabled
          ? VZMACAddress.randomLocallyAdministered().string
          : nil,
        controlPort: request.vsockEnabled ? VMManifest.defaultControlPort : nil
      )
      try manifest.validate()
      let encoder = JSONEncoder()
      encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
      try encoder.encode(manifest).write(to: vmDirectory.manifestURL, options: .atomic)

      try manager.moveItem(at: temporary, to: destination)
      return VMDirectory(url: destination)
    } catch {
      try? manager.removeItem(at: temporary)
      throw error
    }
  }
}
