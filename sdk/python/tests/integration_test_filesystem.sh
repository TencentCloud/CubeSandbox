#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Integration test for filesystem API against a live envd instance.
# Usage: ./integration_test_filesystem.sh <SANDBOX_IP> [PORT]
set -euo pipefail

HOST="${1:?Usage: $0 <SANDBOX_IP> [PORT]}"
PORT="${2:-49983}"
BASE="http://${HOST}:${PORT}"
PASS=0
FAIL=0
TOTAL=0

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    TOTAL=$((TOTAL+1))
    if [ "$expected" = "$actual" ]; then
        green "  PASS: $label"
        PASS=$((PASS+1))
    else
        red "  FAIL: $label (expected='$expected', actual='$actual')"
        FAIL=$((FAIL+1))
    fi
}

assert_contains() {
    local label="$1" needle="$2" haystack="$3"
    TOTAL=$((TOTAL+1))
    if echo "$haystack" | grep -q "$needle"; then
        green "  PASS: $label"
        PASS=$((PASS+1))
    else
        red "  FAIL: $label (expected to contain '$needle')"
        FAIL=$((FAIL+1))
    fi
}

assert_http() {
    local label="$1" expected_code="$2" actual_code="$3"
    assert_eq "$label [HTTP $expected_code]" "$expected_code" "$actual_code"
}

rpc() {
    local method="$1" payload="$2"
    curl -s -w '\n%{http_code}' \
        -X POST "${BASE}/filesystem.Filesystem/${method}" \
        -H "Content-Type: application/json" \
        -H "Connect-Protocol-Version: 1" \
        -d "$payload"
}

rpc_code() {
    echo "$1" | tail -1
}

rpc_body() {
    echo "$1" | sed '$d'
}

echo "========================================"
echo " Filesystem API Integration Tests"
echo " Target: ${BASE}"
echo "========================================"
echo ""

# ── Setup: create test directory via Write endpoint ──────────────────────────
echo "--- Setup ---"
TEST_DIR="/tmp/fs_integration_test_$$"

# 1. MakeDir
echo "[Test 1] MakeDir - create test directory"
RESP=$(rpc "MakeDir" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "MakeDir returns 200" "200" "$CODE"
assert_contains "MakeDir response has entry" "entry" "$BODY"
assert_contains "MakeDir type is DIRECTORY" "FILE_TYPE_DIRECTORY" "$BODY"
echo ""

# 2. MakeDir - nested
echo "[Test 2] MakeDir - create nested directory"
RESP=$(rpc "MakeDir" "{\"path\":\"${TEST_DIR}/subdir\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "MakeDir nested returns 200" "200" "$CODE"
assert_contains "MakeDir nested has subdir name" "subdir" "$BODY"
echo ""

# 3. Write a file via HTTP file API
echo "[Test 3] Write file via /files API"
CODE=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/files?path=${TEST_DIR}/hello.txt&username=root" \
    -H "Content-Type: application/octet-stream" \
    -d "Hello, filesystem API!")
assert_http "Write file returns 200" "200" "$CODE"
echo ""

# 4. Write a second file
echo "[Test 4] Write second file"
CODE=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST "${BASE}/files?path=${TEST_DIR}/subdir/nested.txt&username=root" \
    -H "Content-Type: application/octet-stream" \
    -d "Nested file content")
assert_http "Write nested file returns 200" "200" "$CODE"
echo ""

# 5. Stat - file
echo "[Test 5] Stat - check file metadata"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}/hello.txt\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "Stat file returns 200" "200" "$CODE"
assert_contains "Stat has name" "hello.txt" "$BODY"
assert_contains "Stat type is FILE" "FILE_TYPE_FILE" "$BODY"
assert_contains "Stat has size" "size" "$BODY"
assert_contains "Stat has permissions" "permissions" "$BODY"
echo ""

# 6. Stat - directory
echo "[Test 6] Stat - check directory metadata"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "Stat dir returns 200" "200" "$CODE"
assert_contains "Stat dir type is DIRECTORY" "FILE_TYPE_DIRECTORY" "$BODY"
echo ""

# 7. ListDir - root test dir
echo "[Test 7] ListDir - list test directory"
RESP=$(rpc "ListDir" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "ListDir returns 200" "200" "$CODE"
assert_contains "ListDir has hello.txt" "hello.txt" "$BODY"
assert_contains "ListDir has subdir" "subdir" "$BODY"
echo ""

# 8. ListDir - nested
echo "[Test 8] ListDir - list nested directory"
RESP=$(rpc "ListDir" "{\"path\":\"${TEST_DIR}/subdir\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "ListDir nested returns 200" "200" "$CODE"
assert_contains "ListDir nested has nested.txt" "nested.txt" "$BODY"
echo ""

# 9. Read back file content
echo "[Test 9] Read file via /files API"
CONTENT=$(curl -s "${BASE}/files?path=${TEST_DIR}/hello.txt&username=root")
assert_eq "Read content matches" "Hello, filesystem API!" "$CONTENT"
echo ""

# 10. Move (rename) file
echo "[Test 10] Move - rename file"
RESP=$(rpc "Move" "{\"source\":\"${TEST_DIR}/hello.txt\",\"destination\":\"${TEST_DIR}/renamed.txt\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "Move returns 200" "200" "$CODE"
assert_contains "Move response has renamed.txt" "renamed.txt" "$BODY"
echo ""

# 11. Verify old path is gone (Stat should 404)
echo "[Test 11] Stat - verify old path returns 404"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}/hello.txt\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "Stat old path returns 404" "404" "$CODE"
assert_contains "Stat 404 has not_found code" "not_found" "$BODY"
echo ""

# 12. Verify new path exists
echo "[Test 12] Stat - verify new path exists"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}/renamed.txt\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "Stat renamed returns 200" "200" "$CODE"
assert_contains "Stat renamed has name" "renamed.txt" "$BODY"
echo ""

# 13. Read renamed file content is preserved
echo "[Test 13] Read renamed file content"
CONTENT=$(curl -s "${BASE}/files?path=${TEST_DIR}/renamed.txt&username=root")
assert_eq "Renamed content preserved" "Hello, filesystem API!" "$CONTENT"
echo ""

# 14. Remove file
echo "[Test 14] Remove - delete renamed file"
RESP=$(rpc "Remove" "{\"path\":\"${TEST_DIR}/renamed.txt\"}")
CODE=$(rpc_code "$RESP")
assert_http "Remove file returns 200" "200" "$CODE"
echo ""

# 15. Verify removed file is gone
echo "[Test 15] Stat - verify removed file returns 404"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}/renamed.txt\"}")
CODE=$(rpc_code "$RESP")
assert_http "Stat removed returns 404" "404" "$CODE"
echo ""

# 16. Remove nested file
echo "[Test 16] Remove - delete nested file"
RESP=$(rpc "Remove" "{\"path\":\"${TEST_DIR}/subdir/nested.txt\"}")
CODE=$(rpc_code "$RESP")
assert_http "Remove nested returns 200" "200" "$CODE"
echo ""

# 17. Remove empty subdir
echo "[Test 17] Remove - delete empty subdir"
RESP=$(rpc "Remove" "{\"path\":\"${TEST_DIR}/subdir\"}")
CODE=$(rpc_code "$RESP")
assert_http "Remove subdir returns 200" "200" "$CODE"
echo ""

# 18. ListDir on now-empty test dir
echo "[Test 18] ListDir - empty directory"
RESP=$(rpc "ListDir" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
BODY=$(rpc_body "$RESP")
assert_http "ListDir empty returns 200" "200" "$CODE"
# Should not contain any entries (or entries should be empty)
echo "  Body: $BODY"
echo ""

# 19. Cleanup: remove test dir
echo "[Test 19] Cleanup - remove test directory"
RESP=$(rpc "Remove" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
assert_http "Remove test dir returns 200" "200" "$CODE"
echo ""

# 20. Stat on removed dir confirms 404
echo "[Test 20] Stat - confirm test dir removed"
RESP=$(rpc "Stat" "{\"path\":\"${TEST_DIR}\"}")
CODE=$(rpc_code "$RESP")
assert_http "Stat cleaned up dir returns 404" "404" "$CODE"
echo ""

# ── WatchDir ─────────────────────────────────────────────────────────────────
echo "[Test 21] WatchDir - receive filesystem events"

WATCH_DIR="/tmp/watchdir_test_$$"
curl -s -X POST "${BASE}/filesystem.Filesystem/MakeDir" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  -d "{\"path\":\"${WATCH_DIR}\"}" > /dev/null

# Start watcher in background, write raw binary output
# curl exits 28 on --max-time which is expected; suppress with || true
(python3 -c "
import struct, json, sys
payload = json.dumps({'path': '${WATCH_DIR}'}).encode()
envelope = struct.pack('>bI', 0, len(payload)) + payload
sys.stdout.buffer.write(envelope)
" | curl -s -X POST "${BASE}/filesystem.Filesystem/WatchDir" \
  -H "Content-Type: application/connect+json" \
  -H "Connect-Protocol-Version: 1" \
  --data-binary @- \
  --max-time 6 \
  -o /tmp/watch_raw_$$.bin 2>/dev/null || true) &
CURL_PID=$!
sleep 1

# Trigger events: create, write, remove
curl -s -X POST "${BASE}/files?path=${WATCH_DIR}/w.txt&username=root" \
  -H "Content-Type: application/octet-stream" -d "hello" > /dev/null
sleep 0.5

curl -s -X POST "${BASE}/filesystem.Filesystem/Remove" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  -d "{\"path\":\"${WATCH_DIR}/w.txt\"}" > /dev/null
sleep 0.5

wait $CURL_PID 2>/dev/null

# Parse and verify events
EVENTS=$(python3 -c "
import struct, json
with open('/tmp/watch_raw_$$.bin', 'rb') as f:
    data = f.read()
offset = 0
types = []
while offset + 5 <= len(data):
    flags = data[offset]
    size = struct.unpack('>I', data[offset+1:offset+5])[0]
    offset += 5
    if offset + size > len(data):
        break
    payload = data[offset:offset+size]
    offset += size
    try:
        parsed = json.loads(payload)
        if 'filesystem' in parsed:
            types.append(parsed['filesystem']['type'])
    except:
        pass
print(','.join(types))
" 2>/dev/null)

TOTAL=$((TOTAL+1))
if echo "$EVENTS" | grep -q "EVENT_TYPE_CREATE"; then
    green "  PASS: WatchDir received CREATE event"
    PASS=$((PASS+1))
else
    red "  FAIL: WatchDir missing CREATE event (got: $EVENTS)"
    FAIL=$((FAIL+1))
fi

TOTAL=$((TOTAL+1))
if echo "$EVENTS" | grep -q "EVENT_TYPE_REMOVE"; then
    green "  PASS: WatchDir received REMOVE event"
    PASS=$((PASS+1))
else
    red "  FAIL: WatchDir missing REMOVE event (got: $EVENTS)"
    FAIL=$((FAIL+1))
fi

TOTAL=$((TOTAL+1))
if echo "$EVENTS" | grep -q "EVENT_TYPE_WRITE"; then
    green "  PASS: WatchDir received WRITE event"
    PASS=$((PASS+1))
else
    red "  FAIL: WatchDir missing WRITE event (got: $EVENTS)"
    FAIL=$((FAIL+1))
fi

# Cleanup
curl -s -X POST "${BASE}/filesystem.Filesystem/Remove" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  -d "{\"path\":\"${WATCH_DIR}\"}" > /dev/null
rm -f /tmp/watch_raw_$$.bin
echo ""

# ── Summary ──────────────────────────────────────────────────────────────────
echo "========================================"
if [ $FAIL -eq 0 ]; then
    green " ALL PASSED: $PASS/$TOTAL tests"
else
    red " FAILED: $FAIL/$TOTAL tests"
    yellow " Passed: $PASS/$TOTAL"
fi
echo "========================================"

exit $FAIL
