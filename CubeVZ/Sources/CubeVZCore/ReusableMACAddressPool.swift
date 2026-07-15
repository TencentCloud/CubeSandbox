// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

package struct ReusableMACAddressPool {
  private let capacity: Int
  private var addresses: [String] = []
  private var members: Set<String> = []

  package init(capacity: Int = 1_024) {
    precondition(capacity > 0)
    self.capacity = capacity
  }

  package var count: Int { addresses.count }

  package mutating func take() -> String? {
    guard let address = addresses.popLast() else { return nil }
    members.remove(address)
    return address
  }

  package mutating func recycle(_ address: String) {
    guard members.insert(address).inserted else { return }
    if addresses.count == capacity {
      members.remove(addresses.removeFirst())
    }
    addresses.append(address)
  }
}
