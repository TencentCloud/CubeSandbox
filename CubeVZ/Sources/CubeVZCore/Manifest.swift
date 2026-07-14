// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

public struct VMManifest: Codable, Equatable, Sendable {
  public static let currentSchemaVersion = 1
  public static let defaultControlPort: UInt32 = 1_024

  public var schemaVersion: Int
  public var cpuCount: Int
  public var memoryMiB: UInt64
  public var commandLine: String
  public var kernelFile: String
  public var diskFile: String
  public var initrdFile: String?
  public var networkEnabled: Bool
  public var vsockEnabled: Bool
  public var macAddress: String?
  public var controlPort: UInt32?
  /// Fixed virtiofs device count. Keeping the device topology stable lets a
  /// saved template restore while each sandbox points slots at different host
  /// directories.
  public var volumeShareSlots: Int?

  public init(
    schemaVersion: Int = VMManifest.currentSchemaVersion,
    cpuCount: Int,
    memoryMiB: UInt64,
    commandLine: String,
    kernelFile: String = VMDirectory.kernelFilename,
    diskFile: String = VMDirectory.diskFilename,
    initrdFile: String? = nil,
    networkEnabled: Bool = true,
    vsockEnabled: Bool = true,
    macAddress: String? = nil,
    controlPort: UInt32? = nil,
    volumeShareSlots: Int? = nil
  ) {
    self.schemaVersion = schemaVersion
    self.cpuCount = cpuCount
    self.memoryMiB = memoryMiB
    self.commandLine = commandLine
    self.kernelFile = kernelFile
    self.diskFile = diskFile
    self.initrdFile = initrdFile
    self.networkEnabled = networkEnabled
    self.vsockEnabled = vsockEnabled
    self.macAddress = macAddress
    self.controlPort = controlPort
    self.volumeShareSlots = volumeShareSlots
  }

  public func validate() throws {
    guard schemaVersion == Self.currentSchemaVersion else {
      throw CubeVZError.invalidManifest(
        "schema version \(schemaVersion) is not supported; expected \(Self.currentSchemaVersion)"
      )
    }
    guard cpuCount > 0 else {
      throw CubeVZError.invalidManifest("cpuCount must be greater than zero")
    }
    guard memoryMiB >= 256 else {
      throw CubeVZError.invalidManifest("memoryMiB must be at least 256")
    }
    guard memoryMiB <= UInt64.max / (1_024 * 1_024) else {
      throw CubeVZError.invalidManifest("memoryMiB is too large")
    }
    guard !commandLine.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
      throw CubeVZError.invalidManifest("commandLine must not be empty")
    }
    for (label, value) in [
      ("kernelFile", kernelFile),
      ("diskFile", diskFile),
    ] where !Self.isRelativeFilename(value) {
      throw CubeVZError.invalidManifest("\(label) must be a filename relative to the VM directory")
    }
    if let initrdFile, !Self.isRelativeFilename(initrdFile) {
      throw CubeVZError.invalidManifest(
        "initrdFile must be a filename relative to the VM directory"
      )
    }
    if vsockEnabled, controlPort == 0 {
      throw CubeVZError.invalidManifest("controlPort must be greater than zero")
    }
    if let volumeShareSlots, !(0...16).contains(volumeShareSlots) {
      throw CubeVZError.invalidManifest("volumeShareSlots must be between zero and 16")
    }
  }

  private static func isRelativeFilename(_ value: String) -> Bool {
    !value.isEmpty
      && value != "."
      && value != ".."
      && !value.contains("/")
      && !value.unicodeScalars.contains(Unicode.Scalar(0))
  }
}

public struct VMDirectory: Sendable {
  public static let manifestFilename = "config.json"
  public static let machineIdentifierFilename = "machine-id.bin"
  public static let kernelFilename = "kernel"
  public static let diskFilename = "rootfs.raw"
  public static let initrdFilename = "initrd"
  public static let stateFilename = "machine.vzstate"
  public static let volumeSharesDirectoryName = "volume-shares"

  public let url: URL

  public init(url: URL) {
    self.url = url.standardizedFileURL
  }

  public var manifestURL: URL { url.appendingPathComponent(Self.manifestFilename) }
  public var machineIdentifierURL: URL {
    url.appendingPathComponent(Self.machineIdentifierFilename)
  }
  public var stateURL: URL { url.appendingPathComponent(Self.stateFilename) }
  public var volumeSharesURL: URL {
    url.appendingPathComponent(Self.volumeSharesDirectoryName, isDirectory: true)
  }

  public func volumeShareURL(slot: Int) -> URL {
    volumeSharesURL.appendingPathComponent(String(slot), isDirectory: true)
  }

  public func fileURL(named filename: String) -> URL {
    url.appendingPathComponent(filename)
  }

  public func loadManifest() throws -> VMManifest {
    let data: Data
    do {
      data = try Data(contentsOf: manifestURL)
    } catch {
      throw CubeVZError.filesystem("cannot read \(manifestURL.path): \(error)")
    }

    let manifest: VMManifest
    do {
      manifest = try JSONDecoder().decode(VMManifest.self, from: data)
    } catch {
      throw CubeVZError.invalidManifest("cannot decode \(manifestURL.path): \(error)")
    }
    try manifest.validate()
    return manifest
  }

  public func validateFiles(for manifest: VMManifest) throws {
    let manager = FileManager.default
    let requiredFiles = [
      manifest.kernelFile,
      manifest.diskFile,
      Self.machineIdentifierFilename,
    ]
    for filename in requiredFiles where !manager.fileExists(atPath: fileURL(named: filename).path) {
      throw CubeVZError.filesystem("missing \(fileURL(named: filename).path)")
    }
    if let initrdFile = manifest.initrdFile,
      !manager.fileExists(atPath: fileURL(named: initrdFile).path)
    {
      throw CubeVZError.filesystem("missing \(fileURL(named: initrdFile).path)")
    }
  }
}
