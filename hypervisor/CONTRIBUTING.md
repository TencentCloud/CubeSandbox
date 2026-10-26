# Contributing to Cloud Hypervisor

Cloud Hypervisor is an open source project licensed under the [Apache v2
License](https://opensource.org/licenses/Apache-2.0) and the [BSD 3
Clause](https://opensource.org/licenses/BSD-3-Clause) license. Individual files
contain details of their licensing and changes to that file are under the same
license unless the contribution changes the license of the file. When importing
code from a third party project (e.g. Firecracker or CrosVM) please respect the
license of those projects.

New code should be under the [Apache v2
License](https://opensource.org/licenses/Apache-2.0).

## Coding Style

We follow the [Rust Style](https://github.com/rust-dev-tools/fmt-rfcs/blob/master/guide/guide.md)
convention and enforce it through the Continuous Integration (CI) process calling into `rustfmt`
for each submitted Pull Request (PR).

## Basic Checks

Please consider creating the following hook as `.git/hooks/pre-commit` in order
to ensure basic correctness of your code. You can extend this further if you
have specific features that you regularly develop against.

```sh
#!/bin/sh

cargo fmt -- --check || exit 1
cargo check --locked --all --all-targets --tests || exit 1
cargo clippy --locked --all --all-targets --tests -- -D warnings || exit 1
```

You will need to `chmod +x .git/hooks/pre-commit` to have it run on every
commit you make.

## Certificate of Origin

In order to get a clear contribution chain of trust we use the [signed-off-by language](https://01.org/community/signed-process)
used by the Linux kernel project.

## Patch format

Beside the signed-off-by footer, we expect each patch to comply with the following format:

```
<component>: Change summary

More detailed explanation of your changes: Why and how.
Wrap it to 72 characters.
See http://chris.beams.io/posts/git-commit/
for some more good pieces of advice.

Signed-off-by: <contributor@foo.com>
```

For example:

```
vm-virtio: Reset underlying device on driver request
    
If the driver triggers a reset by writing zero into the status register
then reset the underlying device if supported. A device reset also
requires resetting various aspects of the queue.
    
In order to be able to do a subsequent reactivate it is required to
reclaim certain resources (interrupt and queue EventFDs.) If a device
reset is requested by the driver but the underlying device does not
support it then generate an error as the driver would not be able to
configure it anyway.
    
Signed-off-by: Rob Bradford <robert.bradford@intel.com>
```

## Pull requests

Cloud Hypervisor uses the “fork-and-pull” development model. Follow these steps if
you want to merge your changes to `cloud-hypervisor`:

1. Fork the [cloud-hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) project
   into your github organization.
2. Within your fork, create a branch for your contribution.
3. [Create a pull request](https://help.github.com/articles/creating-a-pull-request-from-a-fork/)
   against the main branch of the Cloud Hypervisor repository.
4. To update your pull request amend existing commits whenever applicable and
   then push the new changes to your pull request branch.
5. Once the pull request is approved it can be integrated.

## Issue tracking

If you have a problem, please let us know. We recommend using
[github issues](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/new) for formally
reporting and documenting them.

To quickly and informally bring something up to us, you can also reach out on [Slack](https://cloud-hypervisor.slack.com).

## Closing issues

You can either close issues manually by adding the fixing commit SHA1 to the issue
comments or by adding the `Fixes` keyword to your commit message:

```
serial: Set terminal in raw mode
    
In order to have proper output from the serial, we need to setup the
terminal in raw mode. When the VM is shutting down, it is also the
VMM responsibility to set the terminal back into canonical mode if we
don't want to get any weird behavior from the shell.
    
Fixes #88
	
Signed-off-by: Sebastien Boeuf <sebastien.boeuf@intel.com>
```

Then, after the corresponding PR is merged, Github will automatically close that issue when parsing the
[commit message](https://help.github.com/articles/closing-issues-via-commit-messages/).
