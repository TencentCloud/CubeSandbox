// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation
import Security
@preconcurrency import Virtualization

public struct DoctorReport: Equatable, Sendable {
  public var architecture: String
  public var osVersion: String
  public var virtualizationSupported: Bool
  public var virtualizationEntitlement: Bool
  public var nestedVirtualizationSupported: Bool

  public init(
    architecture: String,
    osVersion: String,
    virtualizationSupported: Bool,
    virtualizationEntitlement: Bool,
    nestedVirtualizationSupported: Bool
  ) {
    self.architecture = architecture
    self.osVersion = osVersion
    self.virtualizationSupported = virtualizationSupported
    self.virtualizationEntitlement = virtualizationEntitlement
    self.nestedVirtualizationSupported = nestedVirtualizationSupported
  }

  public static func current() -> DoctorReport {
    var systemInfo = utsname()
    uname(&systemInfo)
    let machine = withUnsafePointer(to: &systemInfo.machine) {
      $0.withMemoryRebound(to: CChar.self, capacity: 1) {
        String(cString: $0)
      }
    }
    let nestedVirtualizationSupported: Bool
    if #available(macOS 15.0, *) {
      nestedVirtualizationSupported =
        VZGenericPlatformConfiguration
        .isNestedVirtualizationSupported
    } else {
      nestedVirtualizationSupported = false
    }

    return DoctorReport(
      architecture: machine,
      osVersion: ProcessInfo.processInfo.operatingSystemVersionString,
      virtualizationSupported: VZVirtualMachine.isSupported,
      virtualizationEntitlement: hasVirtualizationEntitlement,
      nestedVirtualizationSupported: nestedVirtualizationSupported
    )
  }

  public var isReady: Bool {
    architecture == "arm64" && virtualizationSupported && virtualizationEntitlement
  }

  private static var hasVirtualizationEntitlement: Bool {
    guard let task = SecTaskCreateFromSelf(nil),
      let value = SecTaskCopyValueForEntitlement(
        task,
        "com.apple.security.virtualization" as CFString,
        nil
      )
    else {
      return false
    }
    return (value as? Bool) == true
  }
}
