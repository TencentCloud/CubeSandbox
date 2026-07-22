# Snapshot Lifecycle Event Contract

> Status: Implemented (producer-side)
> Related issue: [#642 — CubeSandbox Webhook Event Notifications](https://github.com/TencentCloud/CubeSandbox/issues/642)

## Purpose

CubeAPI already emits structured `LogEvent` values for sandbox lifecycle changes, while successful snapshot operations currently write only tracing logs. This change adds producer-side structured events for snapshot operations so any configured logging backend, including a future HTTP Webhook backend, can deliver them consistently.

This proposal does not implement Webhook transport, endpoint configuration, signing, or retries. Those concerns remain in the logging backend.

## Event contract

All events use the existing `LogEvent` envelope, which provides `event`, `timestamp`, and `level`. Operation-specific fields are flattened into the same JSON object.

| Operation | Event | Required fields | Emission point |
| --- | --- | --- | --- |
| Create snapshot | `snapshot.created` | `sandbox_id`, `snapshot_id`, `names` | After `SnapshotService::create` returns successfully |
| Roll back sandbox | `sandbox.rolled_back` | `sandbox_id`, `snapshot_id`, `operation_id`, `status` | After `SnapshotService::rollback` confirms terminal success |
| Delete snapshot | `snapshot.deleted` | `snapshot_id`, `operation_id`, `status`, `sandbox_id` when available | After the snapshot branch of `DELETE /templates/{id}` confirms terminal success |

`snapshot.deleted` reuses the existing snapshot-detail lookup that distinguishes snapshots from regular templates. When CubeMaster returns `origin_sandbox_id`, the event includes it as `sandbox_id`; older records that lack this context still delete successfully and omit the field.

## Emission rules

- Produce one structured event for each successful handler invocation.
- Do not produce a success event when the underlying service returns an error.
- Emit only after the synchronous CubeMaster operation has reached its successful terminal state.
- Keep event names centralized as constants so handlers, subscription validation, tests, and documentation cannot drift.
- Do not include request bodies, secrets, endpoint URLs, or other sensitive values in event fields.
- Event production uses the existing `Logger` interface. Network delivery must remain asynchronous and must not block the API request path.
- Receivers must tolerate duplicate deliveries because a Webhook backend may retry an event after an ambiguous delivery result.

## Code changes

The implementation is expected to touch only the event producer and its tests:

- `CubeAPI/src/logging/`: define the three event-name constants.
- `CubeAPI/src/handlers/snapshots.rs`: emit `snapshot.created` and `sandbox.rolled_back`.
- `CubeAPI/src/handlers/templates.rs`: emit `snapshot.deleted` only in the branch where the identifier resolves to a snapshot.
- `CubeAPI/src/services/snapshots.rs`: preserve the existing snapshot lookup context so delete events can include the originating sandbox when CubeMaster has it.
- CubeAPI tests: verify event name, payload fields, success-only emission, and that regular template deletion does not emit `snapshot.deleted`.

## Non-goals

- Implementing or selecting an HTTP Webhook backend.
- Changing Webhook configuration, HMAC signing, retry, or queue behavior.
- Adding failure events in this change.
- Emitting template-build completion events. Template creation and rebuild handlers return `202 Accepted`; they do not observe the terminal build result and therefore must not emit `template.created` or `template.build.succeeded`.
- Covering automatic pause/resume transitions outside CubeAPI request handlers.

## Acceptance criteria

- Each of the three events is emitted only after its corresponding operation succeeds.
- Payloads contain every required field listed in the event contract.
- Failed operations emit none of these success events.
- Snapshot deletion and regular template deletion remain distinguishable.
- Existing API status codes and response bodies remain unchanged.
- `cargo fmt --manifest-path CubeAPI/Cargo.toml -- --check` passes.
- Relevant CubeAPI unit or integration tests pass.
