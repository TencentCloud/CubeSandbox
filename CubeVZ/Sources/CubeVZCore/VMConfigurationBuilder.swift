// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Foundation
@preconcurrency import Virtualization

public enum VMConfigurationBuilder {
  public static func build(
    directory: VMDirectory,
    manifest: VMManifest,
    consoleInput: FileHandle? = .standardInput,
    consoleOutput: FileHandle? = .standardOutput
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

    do {
      try configuration.validate()
    } catch {
      throw CubeVZError.invalidManifest("Virtualization.framework rejected config: \(error)")
    }
    return configuration
  }
}
