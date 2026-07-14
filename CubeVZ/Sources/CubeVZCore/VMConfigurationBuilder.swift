// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation
@preconcurrency import Virtualization

public enum VMConfigurationBuilder {
  public static func build(
    directory: VMDirectory,
    manifest: VMManifest,
    consoleInput: FileHandle? = .standardInput,
    consoleOutput: FileHandle? = .standardOutput,
    volumeDirectoryURLs: [URL]? = nil
  ) throws -> VZVirtualMachineConfiguration {
    guard VZVirtualMachine.isSupported else {
      throw CubeVZError.unsupported("Apple Virtualization.framework is unavailable")
    }
    #if !arch(arm64)
      throw CubeVZError.unsupported("CubeVZ currently requires Apple Silicon")
    #endif

    try manifest.validate()
    try directory.validateFiles(for: manifest)

    let memorySize = manifest.memoryMiB * 1_024 * 1_024
    guard
      (VZVirtualMachineConfiguration
        .minimumAllowedCPUCount...VZVirtualMachineConfiguration.maximumAllowedCPUCount)
        .contains(manifest.cpuCount)
    else {
      throw CubeVZError.invalidManifest(
        "cpuCount must be between "
          + "\(VZVirtualMachineConfiguration.minimumAllowedCPUCount) and "
          + "\(VZVirtualMachineConfiguration.maximumAllowedCPUCount) on this Mac"
      )
    }
    guard
      (VZVirtualMachineConfiguration
        .minimumAllowedMemorySize...VZVirtualMachineConfiguration.maximumAllowedMemorySize)
        .contains(memorySize)
    else {
      throw CubeVZError.invalidManifest(
        "memoryMiB is outside the range supported by this Mac"
      )
    }

    let configuration = VZVirtualMachineConfiguration()
    configuration.cpuCount = manifest.cpuCount
    configuration.memorySize = memorySize

    let platform = VZGenericPlatformConfiguration()
    let machineIdentifierData: Data
    do {
      machineIdentifierData = try Data(contentsOf: directory.machineIdentifierURL)
    } catch {
      throw CubeVZError.filesystem(
        "cannot read \(directory.machineIdentifierURL.path): \(error)"
      )
    }
    guard
      let machineIdentifier = VZGenericMachineIdentifier(
        dataRepresentation: machineIdentifierData
      )
    else {
      throw CubeVZError.invalidManifest("machine identifier is invalid")
    }
    platform.machineIdentifier = machineIdentifier
    configuration.platform = platform

    let bootLoader = VZLinuxBootLoader(
      kernelURL: directory.fileURL(named: manifest.kernelFile)
    )
    bootLoader.commandLine = manifest.commandLine
    if let initrdFile = manifest.initrdFile {
      bootLoader.initialRamdiskURL = directory.fileURL(named: initrdFile)
    }
    configuration.bootLoader = bootLoader

    let diskAttachment: VZDiskImageStorageDeviceAttachment
    do {
      diskAttachment = try VZDiskImageStorageDeviceAttachment(
        url: directory.fileURL(named: manifest.diskFile),
        readOnly: false,
        cachingMode: .automatic,
        synchronizationMode: .full
      )
    } catch {
      throw CubeVZError.filesystem("cannot attach root disk: \(error)")
    }
    configuration.storageDevices = [
      VZVirtioBlockDeviceConfiguration(attachment: diskAttachment)
    ]

    let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
    serial.attachment = VZFileHandleSerialPortAttachment(
      fileHandleForReading: consoleInput,
      fileHandleForWriting: consoleOutput
    )
    configuration.serialPorts = [serial]
    configuration.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    configuration.memoryBalloonDevices = [
      VZVirtioTraditionalMemoryBalloonDeviceConfiguration()
    ]

    if manifest.networkEnabled {
      let network = VZVirtioNetworkDeviceConfiguration()
      network.attachment = VZNATNetworkDeviceAttachment()
      if let macAddress = manifest.macAddress {
        guard let parsedAddress = VZMACAddress(string: macAddress) else {
          throw CubeVZError.invalidManifest("macAddress is invalid: \(macAddress)")
        }
        network.macAddress = parsedAddress
      } else {
        network.macAddress = VZMACAddress.randomLocallyAdministered()
      }
      configuration.networkDevices = [network]
    }
    if manifest.vsockEnabled {
      configuration.socketDevices = [VZVirtioSocketDeviceConfiguration()]
    }

    let volumeSlots = manifest.volumeShareSlots ?? 0
    if volumeSlots > 0 {
      let urls = volumeDirectoryURLs ?? (0..<volumeSlots).map(directory.volumeShareURL(slot:))
      guard urls.count == volumeSlots else {
        throw CubeVZError.invalidManifest(
          "volume directory count \(urls.count) does not match \(volumeSlots) configured slots"
        )
      }
      configuration.directorySharingDevices = try urls.enumerated().map { slot, url in
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDirectory),
          isDirectory.boolValue
        else {
          throw CubeVZError.filesystem("volume share directory is missing: \(url.path)")
        }
        let sharedDirectory = VZSharedDirectory(url: url, readOnly: false)
        let device = VZVirtioFileSystemDeviceConfiguration(tag: "cube-volume-\(slot)")
        device.share = VZSingleDirectoryShare(directory: sharedDirectory)
        return device
      }
    }

    do {
      try configuration.validate()
      try configuration.validateSaveRestoreSupport()
    } catch {
      throw CubeVZError.invalidManifest("Virtualization.framework rejected config: \(error)")
    }
    return configuration
  }
}
