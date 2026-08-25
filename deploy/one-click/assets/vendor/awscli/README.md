# AWS CLI v2 pack cache

Official AWS CLI v2 installer zips are cached here while building the
one-click bundle. `*.zip` is gitignored; only `VERSION` and `SHA256SUMS`
are committed.

A cache hit with a matching sha256 skips the download. Do not delete this
directory from the pack scripts. Override with `ONE_CLICK_AWSCLI_ZIP`.

打包 one-click 时会把官方 AWS CLI v2 zip 缓存在此目录。`*.zip` 不入库，
仓库只提交 `VERSION` 与 `SHA256SUMS`。sha256 命中则不再下载。打包脚本
不得删除本目录。可用 `ONE_CLICK_AWSCLI_ZIP` 覆盖。
