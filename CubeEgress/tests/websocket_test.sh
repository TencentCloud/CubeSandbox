#!/usr/bin/env bash
# Run with make test-websocket, inside a disposable CubeEgress container.
set -euo pipefail

[[ -f /.dockerenv ]] || { echo "Run make test-websocket instead" >&2; exit 1; }

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
    -addext 'subjectAltName=DNS:localhost' \
    -keyout /etc/cube/ca/cube-root-ca.key \
    -out /etc/cube/ca/cube-root-ca.crt 2>/dev/null
cp /etc/cube/ca/cube-root-ca.crt /etc/cube/ca/placeholder.crt
cp /etc/cube/ca/cube-root-ca.key /etc/cube/ca/placeholder.key

# Keep the real proxy locations and policy checks; use loopback upstreams
# so the test does not require transparent routing or host networking.
sed -e 's/worker_processes 2;/worker_processes 1;/' \
    -e 's/192.168.0.1/127.0.0.1/g' -e 's/ transparent reuseport//g' \
    -e 's@http://\$server_addr:\$server_port@http://127.0.0.1:18080@' \
    -e 's@https://\$server_addr:\$server_port@https://127.0.0.1:18443@' \
    -e 's@/etc/ssl/certs/ca-certificates.crt@/etc/cube/ca/cube-root-ca.crt@' \
    -e '/^[[:space:]]*http[[:space:]]*{/a\    include /tmp/websocket-upstream.conf;' \
    nginx.conf > /tmp/websocket-nginx.conf
cat > /tmp/websocket-upstream.conf <<'NGINX'
    server {
        listen 127.0.0.1:18080;
        listen 127.0.0.1:18443 ssl;
        ssl_certificate /etc/cube/ca/placeholder.crt;
        ssl_certificate_key /etc/cube/ca/placeholder.key;
        access_log off;
        location / {
            content_by_lua_block {
                if ngx.var.uri == "/http" then
                    assert(not ngx.var.http_upgrade)
                    assert(ngx.var.http_connection == "close")
                    ngx.say("ordinary HTTP")
                    return
                end
                local ws = assert(require("resty.websocket.server"):new({timeout = 5000}))
                local data, kind = ws:recv_frame()
                assert(kind == "text", kind)
                assert(ws:send_text(data))
                local _, close_kind = ws:recv_frame()
                assert(close_kind == "close", close_kind)
                assert(ws:send_close())
            }
        }
    }
NGINX

openresty -t -c /tmp/websocket-nginx.conf
openresty -c /tmp/websocket-nginx.conf
trap 'openresty -s quit -c /tmp/websocket-nginx.conf' EXIT

curl --fail --silent --show-error --max-time 5 -X PUT \
    http://127.0.0.1:9091/admin/v1/policies/127.0.0.1 \
    -H 'Content-Type: application/json' -d '{"policy_id":"websocket-test","rules":[
        {"id":"http","match":{"host":"localhost","port":8080,"scheme":"http"},"action":{"allow":true}},
        {"id":"https","match":{"host":"localhost","port":8443,"scheme":"https"},"action":{"allow":true}}
    ]}'

resty --http-conf 'lua_ssl_trusted_certificate /etc/cube/ca/cube-root-ca.crt;' -e '
local http = require "resty.http"
local websocket = require "resty.websocket.client"
for _, endpoint in ipairs({{"http", "ws", 8080}, {"https", "wss", 8443}}) do
    local scheme, ws_scheme, port = unpack(endpoint)
    local address = "127.0.0.1:" .. port
    local client = http.new()
    client:set_timeout(5000)
    -- Non-WebSocket upgrades must keep requests subject to HTTP policy checks.
    for _, upgrade in ipairs({"", "h2c", "websocket, h2c"}) do
        local headers = {Host = "localhost"}
        if upgrade ~= "" then
            headers.Upgrade = upgrade
            headers.Connection = "Upgrade, HTTP2-Settings"
            headers["HTTP2-Settings"] = "AAMAAABkAAQAAP__"
        end
        local response = assert(client:request_uri(scheme .. "://" .. address .. "/http", {
            headers = headers, ssl_server_name = "localhost",
        }))
        assert(response.status == 200, scheme .. " upgrade=" .. upgrade .. ": " .. response.status)
        assert(response.body == "ordinary HTTP\n", response.body)
    end

    local ws = websocket:new({timeout = 5000})
    assert(ws:connect(ws_scheme .. "://" .. address .. "/ws", {
        host = "localhost", server_name = "localhost", ssl_verify = true,
    }))
    assert(ws:send_text("websocket echo"))
    local data, kind = ws:recv_frame()
    assert(kind == "text" and data == "websocket echo", tostring(data))
    assert(ws:send_close())
    local _, close_kind = ws:recv_frame()
    assert(close_kind == "close", tostring(close_kind))
    print(ws_scheme .. ": handshake, echo, close, ordinary HTTP, and upgrade filtering passed")
end
'
