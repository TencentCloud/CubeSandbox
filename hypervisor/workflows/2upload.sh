#!/bin/bash

export CARGO_NET_GIT_FETCH_WITH_CLI=true
source "$HOME/.cargo/env"
rustup default 1.74.0
rustc --version
# 获取 Git 版本短码
git_id=$(git rev-parse --short HEAD)

task_id="${git_id}$(date +%s)"
# "uuid.txt" used for 3callback.sh only
echo "$task_id" > "task_id.txt"

FILE_NAME="cube-src-$task_id.tar.gz"

echo "cargo vendor..."

vendor_out="$(cargo vendor)"
if [ ! -d ".cargo"  ]; then
    mkdir .cargo
fi
echo "$vendor_out" >> ".cargo/config.toml"

pushd ../
# 创建一个名为 "ch-${commit_id}$(date +%s).tar.gz" 的压缩文件
tar -czvf "$FILE_NAME"  --exclude="workspace/target" workspace/

pip install -U cos-python-sdk-v5
python3 workspace/workflows/cos_agent.py upload "$FILE_NAME"
RES=$?
rm "$FILE_NAME"
popd || exit 1
exit $RES
