package.path = "./lua/?.lua;" .. package.path

local cache = {}

function cache:get(key)
    return nil
end

function cache:set(key, value, _ttl)
    return true, nil, false
end

local response_body
local logs = {}

ngx = {
    ERR = "ERR",
    WARN = "WARN",
    var = {
        timeout_min = "500",
        timeout_max = "700",
        cube_proxy_host_ip = "10.0.0.1",
        server_addr = "10.0.0.1",
        http_x_cube_request_id = "test-request",
    },
    shared = {
        local_cache = cache,
    },
    header = {},
    log = function(level, ...)
        logs[#logs + 1] = { level = level, message = table.concat({ ... }) }
    end,
    say = function(body)
        response_body = body
    end,
    exit = function(status)
        error({ status = status }, 0)
    end,
}

local redis_calls = {}
package.loaded["redis_iresty"] = {
    new = function()
        return {
            hgetall = function(_, key)
                local sandbox_id = key:match("([^:]+)$")
                redis_calls[sandbox_id] = (redis_calls[sandbox_id] or 0) + 1
                if sandbox_id == "missing" then
                    return {}, nil
                end
                if sandbox_id == "redis-error" then
                    return nil, "connection refused"
                end
                local is_legacy = key:match("^bypass_host_proxy:") ~= nil
                if sandbox_id == "current-error" then
                    if not is_legacy then
                        return nil, "connection refused"
                    end
                    return {}, nil
                end
                if sandbox_id == "legacy-error" then
                    if is_legacy then
                        return nil, "connection refused"
                    end
                    return {}, nil
                end
                local sandbox_ip = sandbox_id == "recreated"
                    and "192.168.0.20" or "192.168.0.10"
                return {
                    "HostIP", "10.0.0.1",
                    "SandboxIP", sandbox_ip,
                    "AllowPublicTraffic", "true",
                }, nil
            end,
        }
    end,
}

local backend = require "sandbox_backend"

local function expect_exit(status, body, fn)
    response_body = nil
    local ok, exit = pcall(fn)
    assert(not ok, "request must terminate through ngx.exit")
    assert(type(exit) == "table" and exit.status == status,
        "unexpected response status")
    assert(response_body == body, "unexpected response body")
end

-- Only successful misses from every supported Redis route key are authoritative.
expect_exit(404, '{"error":"not found"}', function()
    backend.resolve_backend("missing", "3000")
end)
assert(redis_calls["missing"] == 2,
    "missing route must check every supported Redis key")
assert(logs[#logs].level == ngx.WARN,
    "an expected confirmed miss must not be logged as an error")
assert(logs[#logs].message:find("has no Redis route", 1, true),
    "confirmed-miss warning must identify the missing Redis route")

-- Redis transport failures remain 503.
expect_exit(503, '{"error":"service unavailable"}', function()
    backend.resolve_backend("redis-error", "3000")
end)
assert(redis_calls["redis-error"] == 6,
    "Redis failures must retry each supported key three times")

-- A successful miss from one key must not hide a transport failure from another.
expect_exit(503, '{"error":"service unavailable"}', function()
    backend.resolve_backend("current-error", "3000")
end)
assert(redis_calls["current-error"] == 4)

expect_exit(503, '{"error":"service unavailable"}', function()
    backend.resolve_backend("legacy-error", "3000")
end)
assert(redis_calls["legacy-error"] == 4)

print("sandbox route lookup tests passed")
