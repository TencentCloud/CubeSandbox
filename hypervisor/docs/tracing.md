# Tracing

Cloud Hypervisor has a basic tracing infrastructure, particularly focussed on
the tracing of the initial VM setup.

## Usage

To enabling tracing, build with "tracing" feature. When compiled without the
feature the tracing is compiled out.

```bash
cargo build --features "tracing"
```

And then run Cloud Hypervisor as you wish, the trace will be written to the current directory as `cloud-hypervisor-<pid>.trace`. This is JSON file which you can inspect yourself.

Alternatively you can use the provided script in
`scripts/ch-trace-visualiser.py` to generate an SVG:

For example: 

```bash
scripts/ch-trace-visualiser.py cloud-hypervisor-39466.trace output.svg
```

## Tracing in the codebase

There are existing tracepoints in the code base; extra ones can be added for
more detailed tracing.

The `tracer::trace_scoped!()` macro is used to add the current existing scope
appears as a block in the trace. Other than providing a useful name for the
event nothing else is required from the developer.

The `tracer::start()` and `tracer::end()` functions are already in place for
generating traces of the boot. These can be relocated for focus tracing on a
narrow part of the code base.

A `tracer::trace_point!()` macro is also provided for an instantaneous trace
point however this is not in use in the code base currently nor is handled by
the visualisation script due to the difficulty in representation in the SVG.

