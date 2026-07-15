// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation
import Virtualization

public enum VMTemplateCloner {
  public static func cloneCold(
    template: VMDirectory,
    to destination: URL,
    macAddress: String,
    allowFullCopy: Bool = false
  ) throws -> VMDirectory {
    guard VZMACAddress(string: macAddress) != nil else {
      throw CubeVZError.invalidManifest("macAddress is invalid: \(macAddress)")
    }
    return try cloneFiles(
      template: template,
      to: destination,
      allowFullCopy: allowFullCopy,
      macAddress: macAddress
    )
  }

  private static func cloneFiles(
    template: VMDirectory,
    to destination: URL,
    allowFullCopy: Bool,
    macAddress: String
  ) throws -> VMDirectory {
    let manager = FileManager.default
    let destination = destination.standardizedFileURL
    guard !manager.fileExists(atPath: destination.path) else {
      throw CubeVZError.filesystem("destination already exists: \(destination.path)")
    }

    let manifest = try template.loadManifest()
    try template.validateFiles(for: manifest)

    let temporary = destination.deletingLastPathComponent().appendingPathComponent(
      ".\(destination.lastPathComponent).partial-\(UUID().uuidString)"
    )
    do {
      try manager.createDirectory(at: temporary, withIntermediateDirectories: true)
      let clone = VMDirectory(url: temporary)
      var filenames = [
        manifest.kernelFile,
        manifest.diskFile,
      ]
      if let initrdFile = manifest.initrdFile {
        filenames.append(initrdFile)
      }

      for filename in filenames {
        try FileCloner.clone(
          from: template.fileURL(named: filename),
          to: clone.fileURL(named: filename),
          allowFullCopy: allowFullCopy
        )
      }

      var clonedManifest = manifest
      if clonedManifest.networkEnabled {
        clonedManifest.macAddress = macAddress
      }
      let encoder = JSONEncoder()
      encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
      try encoder.encode(clonedManifest).write(to: clone.manifestURL, options: .atomic)

      let machineIdentifier = VZGenericMachineIdentifier()
      try machineIdentifier.dataRepresentation.write(
        to: clone.machineIdentifierURL,
        options: .atomic
      )

      try manager.moveItem(at: temporary, to: destination)
      let result = VMDirectory(url: destination)
      try result.validateFiles(for: manifest)
      return result
    } catch {
      try? manager.removeItem(at: temporary)
      throw error
    }
  }
}
