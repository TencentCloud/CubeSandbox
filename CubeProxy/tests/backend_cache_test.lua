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

store["sb-1:meta_cached"] = "1"
store["sb-1:HostIP"] = "10.0.0.1"
store["sb-1:SandboxIP"] = "192.168.0.10"
store["sb-1:CreatedAt"] = "1786900000000000000"
store["sb-1:AllowPublicTraffic"] = "false"
store["sb-1:TrafficAccessToken"] = false
store["sb-1:MaskRequestHost"] = false
store["sb-1:49999:backend_ip"] = "192.168.0.10"
store["sb-1:49999:backend_port"] = "49999"
store["sb-2:meta_cached"] = "1"
store["cube_proxy_heartbeat_last_pushed_ms"] = "1"

local deleted = backend_cache.invalidate_sandbox("sb-1")
assert(deleted == 7, "deleted=" .. tostring(deleted))
assert(deleted_keys[1] == "sb-1:meta_cached",
    "meta_cached must be invalidated first")

local fixed_suffixes = {
    "meta_cached",
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

assert(store["sb-1:49999:backend_ip"] == "192.168.0.10")
assert(store["sb-1:49999:backend_port"] == "49999")
assert(store["sb-2:meta_cached"] == "1")
assert(store["cube_proxy_heartbeat_last_pushed_ms"] == "1")

assert(backend_cache.invalidate_sandbox("") == 0)
assert(backend_cache.invalidate_sandbox("missing") == 0)

print("backend_cache_test OK")
