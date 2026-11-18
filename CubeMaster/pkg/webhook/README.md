# Sandbox lifecycle webhooks

[中文](README_zh.md) · [User guide](../../../docs/guide/webhooks.md)

CubeMaster observes sixteen lifecycle success boundaries and sends JSON POSTs to
static cluster-wide subscriptions. Notifications are disabled by default, using
bounded process-local memory and finite retries. Restart/crash can lose pending
work; there is no persistence, takeover, ordering or exactly-once guarantee.
The platform's business Redis remains a dependency.

`App.startWebhooks` validates the existing HTTP YAML and assembles the memory
store and HTTP `Sender`. Disabled, invalid and empty-enabled-subscription paths
install `NopPublisher` without creating delivery resources. No backend YAML keys
are supported.

Go integrations use `NewRuntime(parent, RuntimeOptions, SubscriptionProvider,
DeliveryStore, DeliverySender)`. Runtime takes one deep subscription snapshot;
`Subscription.Target` is interpreted by the sender. `SendResult.Succeeded`
determines success, including non-HTTP senders with `HTTPStatus=0`. Runtime
records finite send diagnostics; HTTP attempt/status/success counters belong to
the HTTP adapter alone. Unknown adapter codes use the `other` metric label.

Backends generate nonempty `Receipt.Token` values for each claim and validate
ownership atomically. `Event.Snapshot` / `RestoreEvent` copy and validate v2
bytes while preserving ID, type, time and exact body; restoration never captures
a new event. No durable backend is implemented here.
Dependencies must be valid, non-typed-nil implementations, exclusive to one
Runtime. Claim returns a valid task/Event and receipt, or an explicit load error.
While Runtime is running, Claim errors are diagnosed and polled again after a
cancellable 50ms wait, each with a fresh operation context. Backends remain
responsible for failed or ambiguous claim state; polling does not automatically
recover or transfer existing claims.

Construction failure leaves dependencies caller-owned. On success Runtime owns
their shutdown: stop ingress, flush writer, `Drain`, cancel/join workers, close
sender, then `Close` store, sharing one two-second budget and any earlier caller
cancellation. Store operations have a finite `StoreOperationTimeout` (App uses
1s), including cancellation acknowledgements. Backends must honor contexts;
sender `Close` must be prompt and local. Failed Complete never increments
terminal counters. Store Close reports actual discards; retained durable tasks
do not count. Memory Close releases local records even after budget expiry.

The executable external-package regressions are in
[`runtime_external_test.go`](runtime_external_test.go),
[`event_snapshot_external_test.go`](event_snapshot_external_test.go) and
[`store_external_test.go`](store_external_test.go).

The [user guide](../../../docs/guide/webhooks.md) covers setup, event timing,
receiver headers/deduplication, field compatibility, HTTP safety and migration
from earlier PR trials. Timeout response deadlines can be fallback values,
killed follows existing handler success, and metadata comes only from trusted
current create input.

Current receiver, component and SDK commands and evidence limits are in the
[test runbook](../../../tests/e2e/webhook/README.md). Historical Redis validation
does not establish acceptance of this memory implementation.
