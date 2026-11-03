#!/usr/bin/env bash
# 真机测试: 冷启动 / 重复触发 / 卸载（需 root, 跑完自动恢复原版）
# 用法: sudo bash deploy/one-click/tests/test-egress-net-reconciler-live.sh
# 输出: /tmp/reconciler-live-test-results.txt

set -uo pipefail  # 不用 -e, 测试中有些命令故意失败

BRANCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOOLBOX=/usr/local/services/cubetoolbox
SERVICE=cube-sandbox-cube-egress-net
RESULTS=/tmp/reconciler-live-test-results.txt
BACKUP=/tmp/cubesandbox-backup-$$
PASS=0; FAIL=0; TOTAL=0

> "$RESULTS"

t()  { TOTAL=$((TOTAL+1)); }
ok() { PASS=$((PASS+1)); echo "  ✓ $1" | tee -a "$RESULTS"; }
no() { FAIL=$((FAIL+1)); echo "  ✗ $1 — $2" | tee -a "$RESULTS"; }
log() { echo "[test] $*" | tee -a "$RESULTS"; }

# ========== 0. 备份原版 ==========
log "=== 0. 备份原版 ==="
mkdir -p "$BACKUP"/{systemd,scripts}
cp "$TOOLBOX"/scripts/systemd/cube-egress-net-{start,stop}.sh "$BACKUP/scripts/" 2>/dev/null
cp /etc/systemd/system/${SERVICE}.service "$BACKUP/systemd/" 2>/dev/null
cp "$TOOLBOX"/scripts/systemd/common.sh "$BACKUP/scripts/" 2>/dev/null
log "备份到 $BACKUP"

# 记录当前规则状态
IPTABLES_HAD_RULES=false
iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY && IPTABLES_HAD_RULES=true
log "原版 iptables 规则状态: $IPTABLES_HAD_RULES"

# ========== 1. 安装新版 ==========
log "=== 1. 安装新版 reconciler ==="
systemctl stop ${SERVICE} 2>/dev/null
# 先清理旧规则（确保干净起点）
"$TOOLBOX"/scripts/cube-egress/cube-egress-iptables-init.sh down 2>/dev/null || true

cp "$BRANCH_DIR"/scripts/systemd/cube-egress-net-reconciler.sh "$TOOLBOX/scripts/systemd/"
cp "$BRANCH_DIR"/scripts/systemd/cube-egress-net-check.sh "$TOOLBOX/scripts/systemd/"
cp "$BRANCH_DIR"/systemd/${SERVICE}.service /etc/systemd/system/
chmod +x "$TOOLBOX/scripts/systemd/cube-egress-net-reconciler.sh"
chmod +x "$TOOLBOX/scripts/systemd/cube-egress-net-check.sh"
systemctl daemon-reload
log "新版已安装, daemon-reload 完成"

# ========== 2. 冷启动测试 ==========
log "=== 2. 冷启动测试 (服务启动 → 等 cube-dev → 规则应用 → STATUS=ready) ==="
t
# 确保 cube-dev 存在（模拟正常启动）
if ! ip link show cube-dev &>/dev/null; then
    log "cube-dev 不存在, 跳过冷启动（需 Cubelet 运行）"
    no "冷启动" "cube-dev 不存在"
else
    systemctl start ${SERVICE}
    sleep 3  # 给 reconciler 时间完成首轮

    if systemctl is-active --quiet ${SERVICE}; then
        STATUS=$(systemctl show ${SERVICE} -p StatusText --value)
        if echo "$STATUS" | grep -q "ready"; then
            ok "冷启动: 服务 running + STATUS 含 ready ($STATUS)"
        else
            no "冷启动" "STATUS 不含 ready: $STATUS"
        fi

        # 验证规则实际存在
        if iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
            ok "冷启动: TPROXY 规则已应用"
        else
            no "冷启动" "TPROXY 规则未找到"
        fi

        # 用 check.sh 验证
        if "$TOOLBOX/scripts/systemd/cube-egress-net-check.sh" &>/dev/null; then
            ok "冷启动: check.sh exit 0"
        else
            no "冷启动" "check.sh exit 非零"
        fi
    else
        no "冷启动" "服务未进入 running"
    fi
fi

# ========== 3. 重复触发测试 ==========
log "=== 3. 重复触发测试 (清除规则 → reconciler 自动重新应用) ==="
t
# 模拟漂移: 手动删除 TPROXY 规则
iptables -t mangle -F CUBE_EGRESS 2>/dev/null
log "已手动清除 CUBE_EGRESS 链"

if ! iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
    log "确认规则已清除"
    sleep 8  # 等 reconciler 的下一个轮询周期（默认 5s）

    if iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
        STATUS=$(systemctl show ${SERVICE} -p StatusText --value)
        ok "重复触发: 规则已自动恢复 ($STATUS)"
    else
        no "重复触发" "规则未自动恢复（等了 8s）"
    fi
else
    no "重复触发" "清除失败, 前提不成立"
fi

# 再做一轮：完整 down 再等恢复
t
"$TOOLBOX"/scripts/cube-egress/cube-egress-iptables-init.sh down 2>/dev/null
log "已完整 down（init script down）"
sleep 8
if iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
    ok "重复触发第二轮: down 后规则自动恢复"
else
    no "重复触发第二轮" "down 后规则未恢复"
fi

# ========== 4. 卸载测试 ==========
log "=== 4. 卸载测试 (stop → ExecStop 清理规则 → 恢复原版 → 验证干净) ==="
t
systemctl stop ${SERVICE}
sleep 2

# ExecStop 应该已清理规则
if ! iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
    ok "卸载: ExecStop 后 TPROXY 规则已清理"
else
    no "卸载" "ExecStop 后规则仍存在"
fi

# 确认服务不 running
if ! systemctl is-active --quiet ${SERVICE}; then
    ok "卸载: 服务已停止"
else
    no "卸载" "服务仍在运行"
fi

# ========== 5. 恢复原版 ==========
log "=== 5. 恢复原版 ==="
cp "$BACKUP/scripts/cube-egress-net-start.sh" "$TOOLBOX/scripts/systemd/" 2>/dev/null
cp "$BACKUP/systemd/${SERVICE}.service" /etc/systemd/system/ 2>/dev/null
systemctl daemon-reload

if [[ "$IPTABLES_HAD_RULES" == true ]]; then
    systemctl start ${SERVICE}
    sleep 2
    if iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q TPROXY; then
        log "原版已恢复, 规则重新应用"
    else
        log "WARN: 原版恢复但规则未应用, 可能需手动重启"
    fi
else
    log "原版已恢复（原无规则, 不重启）"
fi

# ========== 汇总 ==========
echo "" | tee -a "$RESULTS"
echo "===== 真机测试结果: $PASS/$TOTAL 通过 =====" | tee -a "$RESULTS"
[ $FAIL -eq 0 ] && echo "ALL PASS" | tee -a "$RESULTS" || echo "有 $FAIL 个失败" | tee -a "$RESULTS"
echo "完整日志: $RESULTS" | tee -a "$RESULTS"
echo "备份目录: $BACKUP (确认无误后可删)" | tee -a "$RESULTS"
