package.path = "./lua/?.lua;" .. package.path

local store = {}
local cache = {}
local deleted_keys = {}

function cache:get(key)
    return store[key]
end

function cache:set(key, value, _ttl)
    store[key] = value
    return true
end

function cache:delete(key)
    deleted_keys[#deleted_keys + 1] = key
    store[key] = nil
    return true
end

function cache:get_keys(_max)
    error("invalidate_sandbox must not scan the shared cache")
end

ngx = {
    shared = {
        local_cache = cache,
    },
}

local backend_cache = require "backend_cache"

store["sb-1:HostIP"] = "10.0.0.1"
store["sb-1:SandboxIP"] = "192.168.0.10"
store["sb-1:CreatedAt"] = "1786900000000000000"
store["sb-1:AllowPublicTraffic"] = "false"
store["sb-1:TrafficAccessToken"] = false
store["sb-1:MaskRequestHost"] = false
-- The raw per-port host-port mapping is gated by the fixed keys (HostIP /
-- SandboxIP) and overwritten on the next fill, so invalidate need not delete it.
store["sb-1:49999"] = "30001"
store["sb-2:HostIP"] = "10.0.0.5"
store["cube_proxy_heartbeat_last_pushed_ms"] = "1"

local deleted = backend_cache.invalidate_sandbox("sb-1")
assert(deleted == 6, "deleted=" .. tostring(deleted))

local fixed_suffixes = {
    "HostIP",
    "SandboxIP",
    "CreatedAt",
    "AllowPublicTraffic",
    "TrafficAccessToken",
    "MaskRequestHost",
}
for _, suffix in ipairs(fixed_suffixes) do
    assert(store["sb-1:" .. suffix] == nil,
        "fixed route key must be deleted: " .. suffix)
end

-- The per-port mapping is intentionally left in place: with the fixed keys
-- gone the read path misses regardless, and the next fill rewrites it. Other
-- sandboxes and unrelated keys are untouched, and invalidate never scans.
assert(store["sb-1:49999"] == "30001",
    "per-port mapping is gated by the fixed keys, not deleted")
assert(store["sb-2:HostIP"] == "10.0.0.5", "other sandbox untouched")
assert(store["cube_proxy_heartbeat_last_pushed_ms"] == "1")

assert(backend_cache.invalidate_sandbox("") == 0)
assert(backend_cache.invalidate_sandbox("missing") == 0)

print("backend_cache_test OK")
