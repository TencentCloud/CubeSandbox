# Sandbox lifecycle webhooks

[中文](README_zh.md) · [User guide](../../../docs/guide/webhooks.md)

CubeMaster observes six lifecycle success boundaries and sends JSON POSTs to
static cluster-wide subscriptions. Notifications are disabled by default, using
bounded process-local memory and finite retries. Restart/crash can lose pending
work; there is no persistence, takeover, ordering or exactly-once guarantee.
The platform's business Redis remains a dependency.

The [user guide](../../../docs/guide/webhooks.md) covers setup, event timing,
receiver headers/deduplication, field compatibility, HTTP safety and migration
from earlier PR trials. Timeout response deadlines can be fallback values,
killed follows existing handler success, and metadata comes only from trusted
current create input.

Current receiver, component and SDK commands and evidence limits are in the
[test runbook](../../../tests/e2e/webhook/README.md). Historical Redis validation
does not establish acceptance of this memory implementation.
