Safe Path
====================
[![CI](https://github.com/magiclen/path-absolutize/actions/workflows/ci.yml/badge.svg)](https://github.com/magiclen/path-absolutize/actions/workflows/ci.yml)

A library to safely handle filesystem paths, typically for container runtimes.

There are often path related attacks, such as symlink based attacks, TOCTTOU attacks. The `safe-path` crate
provides several functions and utility structures to protect against path resolution related attacks.

## Support

**Operating Systems**:
- Linux

## Reference
- [`filepath-securejoin`](https://github.com/cyphar/filepath-securejoin): secure_join() written in Go.
- [CVE-2021-30465](https://github.com/advisories/GHSA-c3xm-pvg7-gh7r): symlink related TOCTOU flaw in `runC`.

## License

This code is licensed under [Apache-2.0](../../../LICENSE).
