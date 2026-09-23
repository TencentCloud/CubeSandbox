# ECS GRUB regression fixture

`ecs-original.grub` is the original `/etc/default/grub` configuration supplied
from an Alibaba Cloud ECS instance running Ubuntu 24.04. It is the shared input
for both executions; the outputs are not manually edited host snapshots.

- `ecs-old.grub`: output of the unmodified script from `a73d471d` (the parent of
  this PR's original fix). Only `console=tty0` survives.
- `ecs-fixed.grub`: output of the unmodified script from `91c513d4`. Both
  `console=ttyS0,115200` and `console=tty0` survive, in that order.

Both scripts were executed as root in disposable `ubuntu:24.04` containers,
starting from the identical input. The scripts really read, back up, and rewrite
the container's `/etc/default/grub`. Only `update-grub` is replaced with a stub:
bootloader generation and reboot behavior are not tested.

The fixed output is a checked-in expectation, independent of the implementation.
The container test compares the entire output file, checks that the first backup
matches the original, verifies the `update-grub` call, and checks idempotence.
This also detects changes to non-console defaults, including individual
`clearcpuid` entries, and unintended changes to other GRUB settings.

Run from the repository root (Docker required):

```sh
bash deploy/pvm/grub/test-host-grub-ecs.sh

# Reproduce the pre-fix result against its own expected output.
old_script=$(mktemp)
git show a73d471d:deploy/pvm/grub/host_grub_config.sh > "$old_script"
bash deploy/pvm/grub/test-host-grub-ecs.sh "$old_script" \
  deploy/pvm/grub/fixtures/ecs-old.grub

# Negative control: this must fail because the old script loses a console.
bash deploy/pvm/grub/test-host-grub-ecs.sh "$old_script"
rm "$old_script"
```

Fixture changes should be reviewed as configuration changes, not regenerated
automatically to make a failing test pass.
