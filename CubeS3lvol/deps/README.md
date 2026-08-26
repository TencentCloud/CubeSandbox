# deps/

Build dependencies live here, set up by [`setup_dep.sh`](../setup_dep.sh) at the
repo root. **Everything under this directory is git-ignored** (see `.gitignore`);
only this file and `.gitkeep` are tracked.

```sh
./setup_dep.sh            # install whatever is missing
./setup_dep.sh --check    # report only, change nothing
./setup_dep.sh --force spdk
```

| Dependency | Status | Installed to |
|---|---|---|
| SPDK (with DPDK and other submodules) | automatic via `setup_dep.sh` | `deps/spdk`, or `/opt/s3lvol-spdk` |
| aws-c-s3 (AWS CRT, ten projects) | automatic via `setup_dep.sh` | `deps/aws` (sources in `deps/aws-src`), or `/opt/s3lvol-aws-{debug,relwithdebinfo}` |

Local builds use `deps/`; the builder image ships prebuilt trees under `/opt/s3lvol-*` that `setup_dep.sh` reuses when their stamps match the current pin + patches + CRT tags.

## Builder-image prebuilts

`cube-sandbox-builder:ubuntu2004` ships:

- `/opt/s3lvol-spdk` — patched SPDK, `--enable-debug`
- `/opt/s3lvol-aws-debug` / `/opt/s3lvol-aws-relwithdebinfo` — AWS CRT per build type

Each carries a stamp file. `setup_dep.sh` recomputes the hash from the pin, patches, and CRT tags; a match skips clone/build, a mismatch falls back to `deps/`. `make builder-image` compares those hashes to the image labels `org.cubesandbox.s3lvol.{spdk,aws}-stamp` and rebuilds only when they differ.

```sh
./setup_dep.sh --print-stamp spdk
./setup_dep.sh --print-stamp aws
```

SPDK is a shallow fetch of the pinned commit (submodules `--depth 1`).

## Why this directory exists

The build used to assume SPDK lived in `../spdk`. That only holds if the person
cloning this repo also knows to clone SPDK side by side, check out the right
commit, apply the patches under `patches/`, and use the same `./configure`
arguments -- and **none of that is visible from this repository**. Every one of
them, when wrong, fails in a way that is hard to trace back:

- a missing patch -> an `implicit declaration` deep in the build log, or an
  undefined symbol at link time
- different `./configure` arguments -> an undefined symbol at link time
- the wrong version -> the patch will not apply (this one at least reports itself
  clearly)

## Which SPDK commit is pinned

`SPDK_COMMIT` in `setup_dep.sh`, currently `d64c4fa89` (`v26.09-pre-115`).

**That is the upstream baseline, not what `git describe` will print after the
patches are applied.** `patches/apply.sh` uses `git apply` (work tree only), but
`patches/README.md` also allows `git am` -- which leaves a commit behind, so a
hand-built tree ends up ahead of the baseline. This machine is such a tree: the
four patches have been `git am`-ed on top of the baseline, so `HEAD` is
`fbdebfdd2` (the 0004 patch itself), sitting on `10d307f89` (0001),
`16840a99a` (0002), `815b871b6` (0003), and `451b40135` (a patch-splitting
cleanup), all on top of `d64c4fa89`.

Pinned to a commit rather than a tag or master: no SPDK release contains what
this project needs, so the baseline has to be a point on master. And master
moves several times a day; **building against a newer version fails not as "SPDK
is too new" but as patches that no longer apply, or a function whose behaviour
changed silently under us.** Raising the pin should be an explicit action that
comes with a regression run.

## What about an existing `../spdk`

It still works. `mk/s3lvol.common.mk` probes `SPDK_ROOT` (explicit) ->
`deps/spdk` -> `/opt/s3lvol-spdk` -> `../spdk`, so this mechanism **removes a
manual step rather than retiring anyone's existing setup**.

AWS still **does not fall back to a system prefix** such as `/usr/local/aws`.
The lookup is `AWS_INSTALL_DIR` (explicit) -> `deps/aws` (when `.build_type`
matches `AWS_BUILD_TYPE`) -> `/opt/s3lvol-aws-<type>`. The reason system
prefixes stay out is below.

## AWS: why no fallback to a system prefix

A fallback looks friendly and is actually a silent reproducibility hole -- and
it has bitten us once.

`/usr/local/aws` on this machine was installed by **another project**, and its
`aws-c-s3` work tree carried an uncommitted change (excluding 404 from the
`Meta request cannot recover` ERROR log line). So **the same tag `v0.8.7` was
not the same source in the two places**, and the build system picked one by
prefix order without printing which one it picked.

The result was behaviour that varied per machine with no hint at all. Chasing it
down, the cause was even misattributed once (blamed on `CMAKE_BUILD_TYPE`),
precisely because "same tag" made us assume "same source" -- which is the most
expensive part of such a fallback: **it turns "what exactly are we linking"
into a question that has to be excavated.**

So now there is no third path. An explicit `AWS_INSTALL_DIR=/usr/local/aws`
still works perfectly -- that is the escape hatch -- the difference is merely
that **you have to say it out loud**.

For the same reason, only the merged `libaws.a` is accepted, never
`-L<prefix> -laws-c-s3...`: the latter, when a library is missing from the
prefix, **continues searching the system library paths**, and we are back to
"what got linked is unverifiable". Giving the full path to the `.a` either hits
or reports a named error.

That 404 change is **not wanted by this project**: in s3lvol a 404 is supposed
to be treated as an error. So `deps/aws` pulls the pristine upstream tag and
there is no AWS patch under `patches/`. The cost is that the CRT logs s3lvol's
existence probe (a GET of a key that does not exist) as ERROR lines -- noise,
not failure, and the log checks in `run_snapshot_test.sh` / `run_export_test.sh`
/ `run_fs_test.sh` **whitelist only `response status=404`**, not the generic
`Invalid response status from request` text -- that one accompanies every
non-2xx, so filtering on it would swallow 500s and 503s along with it.

## AWS CRT: ten projects, in topological order

Derived from the `aws_build.sh` + `ar.sh` that used to be run by hand. All
versions are pinned to tags (unlike SPDK, which can only pin a commit -- the CRT
is a released library with real version numbers):

| Order | Component | Tag | Notes |
|---|---|---|---|
| 1 | s2n-tls | v1.5.25 | the TLS implementation; without it aws-c-io compiles but has no TLS, and every https request fails at handshake |
| 2 | aws-c-common | v0.12.4 | |
| 3 | aws-checksums | v0.2.6 | |
| 4 | aws-c-cal | v0.9.2 | `-DUSE_OPENSSL=ON` |
| 5 | aws-c-io | v0.22.0 | depends on s2n + cal |
| 6 | aws-c-compression | v0.3.1 | |
| 7 | aws-c-http | v0.10.4 | |
| 8 | aws-c-sdkutils | v0.2.4 | |
| 9 | aws-c-auth | v0.9.1 | SigV4 signing |
| 10 | aws-c-s3 | v0.8.7 | |

**This order is topological, not a preference.** Each project finds the previous
ones through `CMAKE_PREFIX_PATH` pointing at the prefix just installed, so moving
aws-c-io ahead of aws-c-cal fails at configure time.

Finally `ar -M` merges the ten static libraries into a single `libaws.a` (407
object files). Why not ten `-l` flags: they depend on each other, which would
mean ordering them on the link line or wrapping them in `--start-group`; a single
archive makes the problem disappear, and `mk/s3lvol.common.mk` already prefers
`lib64/libaws.a`. `ar -M` rather than `ar q`, because only `ar -M` can `ADDLIB`
a whole archive.

Compared with the old hand-run scripts, four cmake arguments were added, each
with a concrete consequence:

- `-DBUILD_SHARED_LIBS=OFF`: this project links the CRT statically; a shared
  build would simply make `libaws.a` not exist, with no other error.
- `-DCMAKE_POSITION_INDEPENDENT_CODE=ON`: the archive also goes into this
  project's `.so`, otherwise linking `libs3lvol_bdev.so` fails on relocations.
- `-DBUILD_TESTING=OFF`: some of these test suites would drag in extra
  dependencies; we only want the libraries.
- `-DCMAKE_BUILD_TYPE=${AWS_BUILD_TYPE}`, **`Debug` by default**. See below.

### The CRT build type: Debug by default, remembered by a stamp

`AWS_BUILD_TYPE` defaults to `Debug`, the same trade-off as `--enable-debug` on
the SPDK side: while the project is pre-production, being able to step through
the CRT (the S3 request's signing, sending and parsing all live there) is worth
more than the CPU saved. CMake's `Debug` is `-g`: neither `-O` nor `-DNDEBUG`,
so asserts inside the CRT are alive too. Measured `C_FLAGS`:

```
-g -std=gnu99 -fPIC -Wall -Wstrict-prototypes -fno-omit-frame-pointer ...
```

When packaging a release it is the other way around:

```sh
AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --force aws
```

**But it must not be left empty.** CMake's "empty build type" is neither
optimised, nor `NDEBUG`, **and not `-g` either** (it is neither Release nor
Debug), and the CRT's CMakeLists does not supply a default for you -- which is
exactly what the old hand-run script ended up with, getting the worst of both.
So the script always passes it explicitly, and **validates the value before
passing it**: CMake does not complain about an unknown build type, it just looks
up a non-existent `CMAKE_C_FLAGS_<TYPO>` and silently becomes "no optimisation,
no symbols" from one misspelled letter.

`deps/aws/.build_type` records what this prefix was built with. It exists
because `libaws.a` does not say, and "what exactly is linked in here" is a
question this repository has already paid tuition for (previous section).
Without it, changing `AWS_BUILD_TYPE` would **do nothing at all**: the prefix is
complete, so every step is skipped. The behaviour now is:

| stamp | vs `AWS_BUILD_TYPE` | `--check` | `./setup_dep.sh aws` |
|---|---|---|---|
| present | same | `built (..., Debug)` | do nothing |
| present | different | reported, `rc=1` | clear the installed `.a` and build dirs, rebuild |
| absent (hand-built or old) | unknowable | marked `build type unrecorded` | keep as-is + warn, so `--force` works |

The third row is deliberate: rebuilding ten CMake projects for a single missing
file is not worth it.

Rebuilding clears the `.a` rather than adding a switch, because the
continuation logic is per-component and keyed on artifacts -- without clearing
the artifacts, nine of the ten would stay with the old flags. The stamp is
written only at the last step, so an interrupted run leaves "no stamp"
(recoverable), never a wrong value.

### The CRT's 404 ERROR log: a source difference, not a build option

s3lvol decides "does this key exist?" with a GET and reads 404 as the answer;
the code says outright that it is a normal answer (`s3_client_aws.c:1536`). But
aws-c-s3 logs every non-2xx at `[ERROR]`:

```
[ERROR] [S3MetaRequest] - Meta request cannot recover from error 14343
        (Invalid response status from request). response status=404
```

These lines appeared once `deps/aws` was adopted, so the "no unexpected errors
in the log" assertion in `run_snapshot_test.sh` / `run_export_test.sh` stopped
counting them (they take the info branch instead).

**The cause is different source, not `CMAKE_BUILD_TYPE`.** (This section used to
say the latter, and that was wrong: the CRT's log trimming is controlled only by
`AWS_STATIC_LOG_LEVEL`, see `aws-c-common/include/aws/common/logging.h:177`,
which neither build defines -- it has nothing to do with optimisation level or
`NDEBUG`.) The real difference is that the `/usr/local/aws` source carried one
extra `if (response_status != AWS_HTTP_STATUS_CODE_404_NOT_FOUND)` layer -- an
uncommitted local change belonging to another project, as described in the
previous section.

**This project does not want that change**: in s3lvol a 404 is supposed to be
treated as an error. So there is no AWS patch under `patches/`, `deps/aws` uses
the pristine upstream tag, and those ERROR lines in the logs are expected.

On the test side, only `response status=404` is whitelisted, **not** the
`Invalid response status from request` text: that is the generic string of error
14343 and appears with every non-2xx, so filtering on it would swallow 500s and
503s -- which is exactly what that check exists to catch. And note the check is
`grep 'error|failed'` at heart, so **its effectiveness depends entirely on the
exemption list**.
