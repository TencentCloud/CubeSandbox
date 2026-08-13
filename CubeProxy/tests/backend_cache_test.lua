package.path = "./lua/?.lua;" .. package.path

local store = {}
local cache = {}

function cache:get(key)
    return store[key]
end

function cache:set(key, value, _ttl)
    store[key] = value
    return true
end

function cache:delete(key)
    store[key] = nil
    return true
end

function cache:get_keys(_max)
    local keys = {}
    for k, _ in pairs(store) do
        keys[#keys + 1] = k
    end
    return keys
end

ngx = {
    shared = {
        local_cache = cache,
    },
}

local backend_cache = require "backend_cache"

store["sb-1:meta_cached"] = "1"
store["sb-1:SandboxIP"] = "192.168.0.10"
store["sb-1:49999:backend_ip"] = "192.168.0.10"
store["sb-1:49999:backend_port"] = "49999"
store["sb-2:meta_cached"] = "1"
store["cube_proxy_heartbeat_last_pushed_ms"] = "1"

local deleted = backend_cache.delete_sandbox("sb-1")
assert(deleted == 4, "deleted=" .. tostring(deleted))
assert(store["sb-1:meta_cached"] == nil)
assert(store["sb-1:SandboxIP"] == nil)
assert(store["sb-1:49999:backend_ip"] == nil)
assert(store["sb-2:meta_cached"] == "1")
assert(store["cube_proxy_heartbeat_last_pushed_ms"] == "1")

assert(backend_cache.delete_sandbox("") == 0)
assert(backend_cache.delete_sandbox("missing") == 0)

print("backend_cache_test OK")
