// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import Darwin
import Foundation
@preconcurrency import Virtualization

private enum RuntimeEvent: Sendable {
  case terminate
  case guestStopped
  case failed(String)
}

private final class VMDelegate: NSObject, VZVirtualMachineDelegate, @unchecked Sendable {
  let stream: AsyncStream<RuntimeEvent>
  private let continuation: AsyncStream<RuntimeEvent>.Continuation

  override init() {
    let pair = AsyncStream.makeStream(of: RuntimeEvent.self)
    stream = pair.stream
    continuation = pair.continuation
    super.init()
  }

  func guestDidStop(_ virtualMachine: VZVirtualMachine) {
    continuation.yield(.guestStopped)
  }

  func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
    continuation.yield(.failed(error.localizedDescription))
  }

  deinit {
    continuation.finish()
  }
}

private final class SignalMonitor: @unchecked Sendable {
  let stream: AsyncStream<RuntimeEvent>
  private let continuation: AsyncStream<RuntimeEvent>.Continuation
  private let terminateSource: DispatchSourceSignal
  private let interruptSource: DispatchSourceSignal

  init() {
    signal(SIGTERM, SIG_IGN)
    signal(SIGINT, SIG_IGN)

    let pair = AsyncStream.makeStream(of: RuntimeEvent.self)
    stream = pair.stream
    continuation = pair.continuation

    terminateSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
    interruptSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)

    terminateSource.setEventHandler { [continuation] in
      continuation.yield(.terminate)
    }
    interruptSource.setEventHandler { [continuation] in
      continuation.yield(.terminate)
    }
    terminateSource.resume()
    interruptSource.resume()
  }

  deinit {
    terminateSource.cancel()
    interruptSource.cancel()
    continuation.finish()
  }
}

@MainActor
public final class VMRuntime {
  private let virtualMachine: VZVirtualMachine
  private let delegate: VMDelegate
  private let signals: SignalMonitor

  public init(directory: VMDirectory, manifest: VMManifest) throws {
    let configuration = try VMConfigurationBuilder.build(
      directory: directory,
      manifest: manifest
    )
    delegate = VMDelegate()
    signals = SignalMonitor()
    virtualMachine = VZVirtualMachine(configuration: configuration)
    virtualMachine.delegate = delegate
  }

  public func run() async throws {
    try await virtualMachine.start()
    FileHandle.standardError.write(Data("cube-vz: cold boot started\n".utf8))

    FileHandle.standardError.write(
      Data("cube-vz: pid \(ProcessInfo.processInfo.processIdentifier)\n".utf8)
    )

    let event = await waitForEvent()
    switch event {
    case .terminate:
      try await stopWithoutSnapshot()
    case .guestStopped:
      return
    case .failed(let message):
      throw CubeVZError.runtime(message)
    }
  }

  private func waitForEvent() async -> RuntimeEvent {
    await withTaskGroup(of: RuntimeEvent.self) { group in
      group.addTask { [signals] in
        for await event in signals.stream {
          return event
        }
        return .terminate
      }
      group.addTask { [delegate] in
        for await event in delegate.stream {
          return event
        }
        return .guestStopped
      }
      let event = await group.next() ?? .terminate
      group.cancelAll()
      return event
    }
  }

  private func stopWithoutSnapshot() async throws {
    guard virtualMachine.state != .stopped else { return }
    try await virtualMachine.stop()
  }
}
