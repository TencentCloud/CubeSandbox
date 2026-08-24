# Pending envd Fixes Tracked from the Python SDK

> Document purpose: track gaps in envd that the Python SDK has already
> adapted to, but cannot fully exercise until envd implements the
> corresponding server-side behavior. Each entry names the symptom, the
> SDK-side workaround (if any), and the verification path so that the
> gap can be closed and the test restored without further research.
>
> When an entry is resolved: remove the entry, restore the corresponding
> test from `git log -p -- <test-file>`, and update this file's index.

## Index

| # | Issue | envd gap | Tracking test | SDK workaround |
|---|-------|----------|---------------|----------------|
| 1 | Filesystem RPCs ignore `username` body field | envd's `MakeDir` / `ListDir` / `Stat` / `Remove` / `Move` handlers do not drop privileges to the requested user; operations always run as the default user (root) | `tests/e2e/sdk_compat/cases/filesystem/test_metadata_ops.py` (section "cross-user filesystem operations") | None — operations are correct in-process but ownership assertions fail. The user parameter is still forwarded by the SDK and used by command-run paths. |

---

## 1. Filesystem RPCs ignore `username` body field

### Symptom

The Python SDK's `Filesystem` methods (`list`, `stat`, `exists`,
`remove`, `rename`, `make_dir`) accept a `user` parameter. When set,
the SDK includes the username in the request body forwarded to envd:

```
sdk/python/cubesandbox/_filesystem.py:55-57
    if effective_user != DEFAULT_ENVD_USER:
        body["username"] = effective_user
```

E2E tests that use a non-root user with these methods observe that the
target file/directory is owned by root, not the requested user:

```
$ stat -c '%U' /home/sdkcompat/sdk-compat-user-test
root       # expected: sdkcompat
```

The same operations succeed at the syscall level (the file is created
or removed), so the SDK cannot detect the issue from its side — it
correctly forwards the user, but envd does not act on it.

### Where the responsibility lies

**envd.** The handlers in envd's filesystem service must:

1. Parse the `username` field from the request body.
2. Resolve the username to a uid (via `getpwnam` or equivalent).
3. Drop privileges (via `setuid` / `setgid` / `runuser`) before
   performing the filesystem syscall.
4. Handle the `nobody` case (likely a no-op or refuse, depending on
   policy).

This change is security-sensitive and must be reviewed for privilege
boundary correctness (see e2b's analogous implementation in
`packages/envd` for reference).

### SDK side (already done)

- `sdk/python/cubesandbox/_filesystem.py` — `user` parameter forwarded
  in request body for `ListDir`, `Stat`, `MakeDir`, `Remove`, `Move`.
- `sdk/python/cubesandbox/_filesystem.py:32-35` — query param dropped
  in favor of body to match envd's proto contract.
- All other SDKs (Go, Node) are aligned: filesystem RPCs do not pass
  a user parameter, and the e2b spec does not require it. Once envd
  supports it, the Go/Node SDKs can opt in.

### Verification path when envd is fixed

1. Confirm envd drops privileges and the file is created with the
   correct ownership under a non-root user.
2. Restore the test in
   `tests/e2e/sdk_compat/cases/filesystem/test_metadata_ops.py`
   (section "cross-user filesystem operations") from `git log -p` —
   it exercises `make_dir` / `list` / `stat` / `exists` / `rename` /
   `remove` as a non-root user and asserts the owner via
   `stat -c '%U'`.
3. Re-run the full e2e suite under
   `--sdk-e2e-backends cubesandbox,e2b`. The test should pass on
   cubesandbox; e2b behavior depends on the upstream e2b envd.
4. Remove this entry from the table above and update the doc header.

### Related work

- `tests/e2e/sdk_compat/cases/filesystem/test_filesystem_user.py` —
  user-isolation tests that work today by going through `run_command`
  (which uses `Authorization: Basic` and IS honored by envd) rather
  than the filesystem RPCs. These tests are the SDK-side equivalent
  of the gap above: they verify the user parameter is correctly
  forwarded at the process layer. They will not regress when envd is
  fixed.
