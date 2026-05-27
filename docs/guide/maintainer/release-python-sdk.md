# Releasing the Python SDK

This guide explains how to publish the `cubesandbox` Python package on PyPI from CI.

The SDK source lives in [`sdk/python/`](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python).
Releases are driven by the [`release-python-sdk.yml`](https://github.com/TencentCloud/CubeSandbox/blob/main/.github/workflows/release-python-sdk.yml)
GitHub Actions workflow.

## Prerequisites

- A maintainer-level GitHub account on the repository (push access for tags).
- The repository secret `CUBE_PYPI_TOKEN` must be configured with a valid PyPI API token.
  - For first-ever release: a user-scoped token can be used. Replace it with a project-scoped token (`Scope = Project: cubesandbox`) immediately after the first successful publish.
  - Optionally, add a TestPyPI token under the same `CUBE_PYPI_TOKEN` name in a separate environment if you want to dry-run against TestPyPI.

## Versioning rules

- Follow [Semantic Versioning](https://semver.org/): `MAJOR.MINOR.PATCH` (pre-releases such as `0.3.0rc1` are also accepted by PyPI).
- Version is the **single source of truth** in `sdk/python/cubesandbox/__init__.py` (`__version__ = "X.Y.Z"`).
  `pyproject.toml` reads it dynamically via `[tool.setuptools.dynamic].version`.
- The git tag must match the package version exactly:
  - Tag format: `python-sdk-vX.Y.Z`
  - The release workflow fails if `tag` ≠ `__version__`.

## Tag conventions and workflow isolation

| Workflow | Tag pattern |
|---|---|
| `release-one-click.yml` (main bundle release) | `*` excluding `python-sdk-v*` |
| `release-python-sdk.yml` (Python SDK on PyPI) | `python-sdk-v*` |

Both workflows are mutually exclusive: a `python-sdk-v*` tag never triggers the one-click bundle, and a regular `vX.Y.Z` tag never triggers a PyPI publish.

## Standard release procedure

1. **Bump the version** on `main` in a regular PR:

   ```python
   # sdk/python/cubesandbox/__init__.py
   __version__ = "0.2.1"
   ```

   Update `sdk/python/README.md` and the changelogs (`docs/changelog.md`, `docs/zh/changelog.md`) if the release contains user-visible changes. Get the PR reviewed and merged like any other change.

2. **(Optional but recommended) Dry-run via TestPyPI.** Open the
   [Release Python SDK workflow](https://github.com/TencentCloud/CubeSandbox/actions/workflows/release-python-sdk.yml),
   click **Run workflow**, and pick `target: testpypi`. Then verify install:

   ```bash
   python -m venv /tmp/venv && source /tmp/venv/bin/activate
   pip install -i https://test.pypi.org/simple/ \
     --extra-index-url https://pypi.org/simple/ \
     "cubesandbox==0.2.1"
   python -c "import cubesandbox; print(cubesandbox.__version__)"
   ```

3. **Tag and push** from the merge commit on `main`:

   ```bash
   git checkout main && git pull
   git tag python-sdk-v0.2.1
   git push origin python-sdk-v0.2.1
   ```

4. **Watch the workflow.** The `Release Python SDK` workflow runs automatically and:
   - Validates `tag` ↔ `__version__` consistency.
   - Builds `sdist` and `wheel` with `python -m build`.
   - Validates package metadata via `twine check --strict`.
   - Publishes to PyPI using `CUBE_PYPI_TOKEN`.
   - Creates the matching GitHub Release and attaches the built artifacts.

5. **Smoke-test the release** from a clean environment:

   ```bash
   python -m venv /tmp/venv && source /tmp/venv/bin/activate
   pip install "cubesandbox==0.2.1"
   python -c "from cubesandbox import Sandbox; print('ok')"
   ```

## Recovering from a bad release

PyPI **does not allow re-uploading the same version**, even after deletion. If a published version has a serious bug:

1. Yank the affected version on PyPI (Project → Manage → Releases → Yank). Yanked versions stay installable when pinned but are skipped by `pip install cubesandbox`.
2. Bump the version (e.g. `0.2.1` → `0.2.2`) and follow the standard release procedure.
3. If the workflow itself failed before publish, simply fix the issue, delete the failed tag (`git push --delete origin python-sdk-v0.2.1 && git tag -d python-sdk-v0.2.1`), and re-tag.

## Future improvements

- **Trusted Publishing (OIDC).** Replace the `CUBE_PYPI_TOKEN` secret with PyPI Trusted Publishing. Add the workflow as a trusted publisher on the PyPI project page, set `permissions: id-token: write` on the publish job, and remove the `password:` input. Tokens then no longer need to be rotated.
- **Test gating.** Run `pytest -m "not e2e"`, `ruff check`, and `mypy` as a prerequisite job for `build-and-publish`, so a release cannot be cut from a broken state.
