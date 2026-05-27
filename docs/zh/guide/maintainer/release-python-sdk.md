# 发布 Python SDK

本文说明如何通过 CI 将 `cubesandbox` Python 包发布到 PyPI。

SDK 源码位于 [`sdk/python/`](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python)，
发布流水线为 [`release-python-sdk.yml`](https://github.com/TencentCloud/CubeSandbox/blob/main/.github/workflows/release-python-sdk.yml)。

## 前置条件

- 仓库的维护者权限（可以推送 tag）。
- 仓库 Secret `CUBE_PYPI_TOKEN` 已配置为有效的 PyPI API Token。
  - 首次发版时可以使用 user-scoped token；首次发布成功后**立即**替换成 project-scoped token（`Scope = Project: cubesandbox`）。
  - 如需先在 TestPyPI dry-run，可在独立 environment 下用同名 secret 配置 TestPyPI token。

## 版本规范

- 遵循 [SemVer](https://semver.org/) `MAJOR.MINOR.PATCH`，预发布版本（如 `0.3.0rc1`）也被 PyPI 接受。
- 版本号以 `sdk/python/cubesandbox/__init__.py` 中的 `__version__ = "X.Y.Z"` 为**唯一来源**，`pyproject.toml` 通过 `[tool.setuptools.dynamic].version` 动态读取，无需重复维护。
- git tag 必须与版本号严格一致：
  - tag 格式：`python-sdk-vX.Y.Z`
  - 不一致时，发布工作流会直接失败。

## tag 约定与流水线隔离

| 工作流 | 触发 tag |
|---|---|
| `release-one-click.yml`（主包一键部署 release） | `*`，但排除 `python-sdk-v*` |
| `release-python-sdk.yml`（Python SDK 发布到 PyPI） | `python-sdk-v*` |

两条流水线互不重叠：`python-sdk-v*` 不会触发 one-click bundle，普通的 `vX.Y.Z` 也不会触发 PyPI 发布。

## 标准发版流程

1. **在 `main` 分支提一个常规 PR 升版本号**：

   ```python
   # sdk/python/cubesandbox/__init__.py
   __version__ = "0.2.1"
   ```

   如果发布内容有用户可见变更，同步更新 `sdk/python/README.md`、`docs/changelog.md`、`docs/zh/changelog.md`。然后走正常的 review 与合入流程。

2. **（可选，强烈建议）TestPyPI dry-run。** 打开
   [Release Python SDK 工作流](https://github.com/TencentCloud/CubeSandbox/actions/workflows/release-python-sdk.yml)，
   点击 **Run workflow**，选 `target: testpypi`。完成后验证安装：

   ```bash
   python -m venv /tmp/venv && source /tmp/venv/bin/activate
   pip install -i https://test.pypi.org/simple/ \
     --extra-index-url https://pypi.org/simple/ \
     "cubesandbox==0.2.1"
   python -c "import cubesandbox; print(cubesandbox.__version__)"
   ```

3. **在 `main` 上的合入 commit 打 tag 并推送**：

   ```bash
   git checkout main && git pull
   git tag python-sdk-v0.2.1
   git push origin python-sdk-v0.2.1
   ```

4. **观察工作流**。`Release Python SDK` 自动执行：
   - 校验 `tag` 与 `__version__` 一致；
   - 用 `python -m build` 构建 sdist 和 wheel；
   - `twine check --strict` 校验包元信息；
   - 用 `CUBE_PYPI_TOKEN` 发布到 PyPI；
   - 创建对应的 GitHub Release 并附上构建产物。

5. **在干净环境冒烟验证**：

   ```bash
   python -m venv /tmp/venv && source /tmp/venv/bin/activate
   pip install "cubesandbox==0.2.1"
   python -c "from cubesandbox import Sandbox; print('ok')"
   ```

## 发版异常处理

PyPI **不允许同一版本号重复上传**，即使已删除。如果某个版本上线后发现严重问题：

1. 在 PyPI 上 yank 该版本（Project → Manage → Releases → Yank）。被 yank 的版本依然可以通过精确 pin 安装，但 `pip install cubesandbox` 会跳过它。
2. 升一位版本（例如 `0.2.1` → `0.2.2`），再走一遍标准发版流程。
3. 如果是工作流自身在发布前就失败，直接修问题，删掉失败的 tag（`git push --delete origin python-sdk-v0.2.1 && git tag -d python-sdk-v0.2.1`），再重新打 tag。

## 后续可改进项

- **切换到 PyPI Trusted Publishing（OIDC）**。在 PyPI 项目页将本工作流加为 trusted publisher，给 publish job 加 `permissions: id-token: write` 并移除 `password:` 字段，从此不再需要 `CUBE_PYPI_TOKEN`，也无需轮换。
- **加测试门禁**。在 `build-and-publish` 之前增加一个 job 跑 `pytest -m "not e2e"`、`ruff check`、`mypy`，避免从坏状态发版。
