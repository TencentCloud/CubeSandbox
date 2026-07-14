// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation

public enum VMTemplateCloner {
  public static func clone(
    template: VMDirectory,
    to destination: URL,
    allowFullCopy: Bool = false
  ) throws -> VMDirectory {
    let manager = FileManager.default
    let destination = destination.standardizedFileURL
    guard !manager.fileExists(atPath: destination.path) else {
      throw CubeVZError.filesystem("destination already exists: \(destination.path)")
    }

    let manifest = try template.loadManifest()
    try template.validateFiles(for: manifest)
    guard manager.fileExists(atPath: template.stateURL.path) else {
      throw CubeVZError.filesystem("template has no saved state: \(template.stateURL.path)")
    }

    let temporary = destination.deletingLastPathComponent().appendingPathComponent(
      ".\(destination.lastPathComponent).partial-\(UUID().uuidString)"
    )
    do {
      try manager.createDirectory(at: temporary, withIntermediateDirectories: true)
      let clone = VMDirectory(url: temporary)
      var filenames = [
        VMDirectory.manifestFilename,
        VMDirectory.machineIdentifierFilename,
        manifest.kernelFile,
        manifest.diskFile,
        VMDirectory.stateFilename,
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

      for slot in 0..<(manifest.volumeShareSlots ?? 0) {
        try manager.createDirectory(
          at: clone.volumeShareURL(slot: slot),
          withIntermediateDirectories: true
        )
      }

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
