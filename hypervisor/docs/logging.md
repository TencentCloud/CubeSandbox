# Logging

The target audience of this document is both:

* Developers who want to understand what log level to use and when,
* Users who want to debug issues with running their workloads in Cloud Hypervisor

## Control

The number of `-v` parameters passed to the `cloud-hypervisor` binary will determine the log level. Currently the default is log messages up to `WARN:` (`warn!`) are included by default. The `--log-file` allows the log to be sent to a location other than `stderr`.

## Levels

### `error!()`

For immediate, unrecoverable errors where it does not make sense for the execution to continue as the behaviour of the VM is considerably impacted.

Cloud Hypervisor should exit shortly after reporting this error (with a non-zero exit code). Generally this should be used during initial construction of the VM state before the virtual CPUs have begun running code.

A typical situation where this might occur is when the user is using command line options that conflict with each other or is trying to use a file that is not present on the filesystem.

Users should react to this error by checking their initial VM configuration.

### `warn!()`

A serious problem has occurred but the execution of the VM can continue although some functionality might be impacted.

A typical example of where this level of message should be generated is during an API call request that cannot be fulfilled.

The user should investigate the meaning of this warning and take steps to ensure the correct functionality.


### `info!()`

Use `-v` to enable.

This level is for the benefit of developers. It should be used for sporadic and infrequent messages. The same message should not "spam" the logs. The VM should be usable when this level of debugging is enabled and trying to use `stdin/stdout` and the logs are going to `stderr`.

### `debug!()`

Use `-vv` to enable.

For the most verbose of logging messages. It is acceptable to "spam" the log with repeated invocations of the same message. This level of logging would be combined with `--log-file`.