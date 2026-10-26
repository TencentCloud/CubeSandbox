# Balloon

Cloud Hypervisor implements a balloon device based on the VIRTIO specification.
Its main purpose is to provide the host a way to reclaim memory by controlling
the amount of memory visible to the guest. But it also provides some interesting
features related to guest memory management.

## Parameters

`BalloonConfig` (known as `--balloon` from the CLI perspective) contains the
list of parameters available for the balloon device.

```rust
struct BalloonConfig {
    pub size: u64,
    pub deflate_on_oom: bool,
    pub free_page_reporting: bool,
}
```

```
--balloon <balloon>	Balloon parameters "size=<balloon_size>,deflate_on_oom=on|off,free_page_reporting=on|off"
```

### `size`

Size of the balloon device. It is subtracted from the VM's total size. For
instance, if creating a VM with 4GiB of RAM, along with a balloon of 1GiB, the
guest will be able to use 3GiB of accessible memory. The guest sees all the RAM
and unless it is balloon enlightened is entitled to all of it.

This parameter is mandatory.

Value is an unsigned integer of 64 bits corresponding to the balloon size in
bytes.

_Example_

```
--balloon size=1G
```

### `deflate_on_oom`

Allow the guest to deflate the balloon if running Out Of Memory (OOM). Assuming
the balloon size is greater than 0, this means the guest is allowed to reduce
the balloon size all the way down to 0 if this can help recover from the OOM
event.

This parameter is optional.

Value is a boolean set to `off` by default.

_Example_

```
--ballloon size=2G,deflate_on_oom=on
```

### `free_page_reporting`

Allow the guest to report lists of free pages. This feature doesn't require the
balloon to be of any specific size as it doesn't impact the balloon size. The
guest can let the VMM know about pages that are free after they have been used.
Based on this information, the VMM can advise the host that it doesn't need
these pages anymore.

This parameter is optional.

Value is a boolean set to `off` by default.

_Example_

```
--ballloon size=0,free_page_reporting=on
```
