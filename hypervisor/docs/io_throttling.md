# I/O Throttling

Cloud Hypervisor now supports I/O throttling on virtio-block and virtio-net
devices. This support is based on the [`rate-limiter` module](https://github.com/firecracker-microvm/firecracker/tree/master/src/rate_limiter)
from Firecracker. This document explains the user interface of this
feature, and highlights some internal implementations that can help users
better understand the expected behavior of I/O throttling in practice.

Cloud Hypervisor allows to limit both the I/O bandwidth (e.g. bytes/s)
and I/O operations (ops/s) independently. For virtio-net devices, while
sharing the same "rate limit" from user inputs (on both bandwidth and
operations), the RX and TX queues are throttled independently.
To limit the I/O bandwidth, Cloud Hypervisor
provides three user options, i.e., `bw_size` (bytes), `bw_one_time_burst`
(bytes), and `bw_refill_time` (ms). Both `bw_size` and `bw_refill_time`
are required, while `bw_one_time_burst` is optional.
Internally, these options define a TokenBucket with a maximum capacity
(`bw_size` bytes), an initial burst size (`bw_one_time_burst`) and an
interval for refilling purposes (`bw_refill_time`). The "refill-rate" is
`bw_size` bytes per `bw_refill_time` ms, and it is the constant rate at
which the tokens replenish. The refill process only starts happening
after the initial burst budget is consumed. Consumption from the token
bucket is unbounded in speed which allows for bursts bound in size by
the amount of tokens available. Once the token bucket is empty,
consumption speed is bound by the "refill-rate". Similarly, Cloud
Hypervisor provides another three options for limiting I/O operations,
i.e., `ops_size` (I/O operations), `ops_one_time_burst` (I/O operations),
and `ops_refill_time` (ms).

One caveat in the I/O throttling is that every-time the bucket gets
empty, it will stop I/O operations for a fixed amount of time
(`cool_down_time`). The `cool_down_time` now is fixed at `100 ms`, it
can have big implications to the actual rate limit (which can be a lot
different the expected "refill-rate" derived from user inputs). For
example, to have a 1000 IOPS limit on a virtio-blk device, users should
be able to provide either of the following two options:
`ops_size=1000,ops_refill_time=1000` or
`ops_size=10,ops_refill_time=10`. However, the actual IOPS limits are
likely to be ~1000 IOPS and ~100 IOPS respectively. The reason is the
actual rate limit users get can be as low as
`ops_size/(ops_refill_time+cool_down_time)`. As a result, it is
generally advisable to keep `bw/ops_refill_time` larger than `100 ms`
(`cool_down_time`) to make sure the actual rate limit is close to users'
expectation ("refill-rate").
