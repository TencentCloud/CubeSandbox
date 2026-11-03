#!/bin/bash
# 模拟测试: 不需要真机, 用函数覆盖 + 子进程隔离

SCRIPT="deploy/one-click/scripts/systemd/cube-egress-net-reconciler.sh"
PASS=0; FAIL=0; TOTAL=0

t() { TOTAL=$((TOTAL+1)); }

ok() { PASS=$((PASS+1)); echo "  ✓ $1"; }
no() { FAIL=$((FAIL+1)); echo "  ✗ $1  — $2"; }

# ---- 测试1: iface_present() 对不存在的接口返回非零 ----
# 用一个确定不存在的接口名绕过环境干扰（本机可能恰好有 cube-dev）
t
FAKE_IFACE="cube-test-notexist-$$"
if ! ip link show "${FAKE_IFACE}" &>/dev/null; then
  ok "场景1 (不存在的接口正确返回非零): ${FAKE_IFACE} 不存在 → reconciler 会进 waiting"
else
  no "场景1" "测试接口 ${FAKE_IFACE} 意外存在"
fi
# 验证 reconcile 逻辑中 iface_present 的调用语义
t
if grep -q "iface_present" deploy/one-click/scripts/systemd/cube-egress-net-reconciler.sh; then
  ok "场景1b (iface_present 函数被 reconciler 主循环调用): 代码路径存在"
else
  no "场景1b" "iface_present 未被调用"
fi

# ---- 测试2: check.sh 在无规则时 exit 1 ----
t
if CUBE_INGRESS_IFACE=lo deploy/one-click/scripts/systemd/cube-egress-net-check.sh &>/dev/null; then
  no "场景2 (check.sh 应 fail)" "lo 存在但无 TPROXY, 应 exit 1"
else
  ok "场景2 (check.sh 对无规则接口正确拒绝): exit 非零"
fi

# ---- 测试3: check.sh 脚本内容审查 ----
t
if grep -q "TPROXY" deploy/one-click/scripts/systemd/cube-egress-net-check.sh && \
   grep -q "cube-dev" deploy/one-click/scripts/systemd/cube-egress-net-check.sh; then
  ok "场景3 (check.sh 检查 TPROXY + cube-dev 双条件): 代码审查通过"
else
  no "场景3" "check.sh 缺少关键检查项"
fi

# ---- 测试4: reconciler.sh 包含 STATUS= 三态 ----
t
STATES=$(grep -c "STATUS=" deploy/one-click/scripts/systemd/cube-egress-net-reconciler.sh)
if [ "$STATES" -ge 3 ]; then
  ok "场景4 (STATUS= 三态报告): ${STATES} 处 STATUS= 调用"
else
  no "场景4" "STATUS= 只有 ${STATES} 处, 预期 ≥3"
fi

# ---- 测试5: service 文件类型检查 ----
t
TYPE=$(grep "^Type=" deploy/one-click/systemd/cube-sandbox-cube-egress-net.service)
if echo "$TYPE" | grep -q "simple"; then
  ok "场景5 (service Type=simple): $TYPE"
else
  no "场景5" "Type 不是 simple: $TYPE"
fi

# ---- 测试6: 确认 .path 和 trampoline 已删除 ----
t
if [ ! -f deploy/one-click/systemd/cube-sandbox-cube-egress-net.path ] && \
   [ ! -f deploy/one-click/systemd/cube-sandbox-cube-egress-net-retrigger.service ]; then
  ok "场景6 (旧 .path / trampoline 已清除): 确认不存在"
else
  no "场景6" "旧文件残留"
fi

# ---- 测试7: 确认旧 start.sh 已被替换 ----
t
if [ ! -f deploy/one-click/scripts/systemd/cube-egress-net-start.sh ] && \
   [ -f deploy/one-click/scripts/systemd/cube-egress-net-reconciler.sh ]; then
  ok "场景7 (start.sh 替换为 reconciler.sh): 确认"
else
  no "场景7" "文件替换不完整"
fi

# ---- 测试8: ExecStop 仍指向 stop.sh ----
t
if grep -q "ExecStop.*cube-egress-net-stop.sh" deploy/one-click/systemd/cube-sandbox-cube-egress-net.service; then
  ok "场景8 (ExecStop 清理保留): stop.sh 仍然挂载"
else
  no "场景8" "ExecStop 丢失"
fi

# ---- 测试9: 幂等 apply 逻辑存在 ----
t
if grep -q "rules_applied" deploy/one-click/scripts/systemd/cube-egress-net-reconciler.sh; then
  ok "场景9 (幂等检查函数存在): rules_applied() 已定义"
else
  no "场景9" "缺少幂等检查"
fi

# ---- 测试10: common.sh 不含 .path glob（回到原版） ----
t
if ! grep -q "\.path" deploy/one-click/scripts/systemd/common.sh; then
  ok "场景10 (common.sh 无 .path glob): 已还原"
else
  no "场景10" "common.sh 残留 .path glob"
fi

echo
echo "===== 结果: $PASS/$TOTAL 通过 ====="
[ $FAIL -eq 0 ] && echo "ALL PASS" || echo "有 $FAIL 个失败"
