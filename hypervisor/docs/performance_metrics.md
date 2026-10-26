# Performance Metrics

Cloud Hypervisor provides a [performance metrics](https://github.com/cloud-hypervisor/cloud-hypervisor/tree/main/performance-metrics)
binary for users to generate metrics data from their own
environment. This document describes how to generate metrics data
quickly by using Cloud Hypervisor's development script,
e.g. `dev_cli.sh`. The only prerequisite is [Docker installation](https://docs.docker.com/engine/install/).
Please note that upon its first invocation, this script will pull a
fairly large container image.

To generate metrics data for all available performance tests (including
boot time, block I/O throughput, and network throughput & latency) and
output the result into a json file:

```
$ ./scripts/dev_cli.sh tests --metrics -- -- --report-file /tmp/metrics.json
```

To get a list of available performance tests:

```
$ ./scripts/dev_cli.sh tests --metrics -- -- --list-tests
```

To generate metrics data for selected performance tests, e.g. boot time only:

```
$ ./scripts/dev_cli.sh tests --metrics -- -- --report-file /tmp/metrics.json --test-filter boot_time
```
