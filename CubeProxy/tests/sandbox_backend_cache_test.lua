package.path = "./lua/?.lua;" .. package.path

local cache_values = {}
local cache = {}

function cache:get(key)
    return cache_values[key]
end

function cache:set(key, value, _ttl)
    cache_values[key] = value
    return true
end

ngx = {
    ERR = "ERR",
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
    log = function() end,
}

local redis_calls = 0
package.loaded["redis_iresty"] = {
    new = function()
        return {
            hgetall = function(_, key)
                redis_calls = redis_calls + 1
                local sandbox_id = key:match("([^:]+)$")
                local values = {
                    "HostIP", "10.0.0.1",
                    "SandboxIP", "192.168.0.10",
                    "AllowPublicTraffic", "true",
                }
                if sandbox_id == "with-mask" then
                    values[#values + 1] = "MaskRequestHost"
                    values[#values + 1] = "localhost:${PORT}"
                end
                return values, nil
            end,
        }
    end,
}

local backend = require "sandbox_backend"

local host, port, mask = backend.resolve_backend("with-mask", "3000")
assert(host == "192.168.0.10")
assert(port == "3000")
assert(mask == "localhost:${PORT}")
assert(redis_calls == 1)

-- Simulate independent LRU eviction of only the optional mask key. The next
-- request must reload Redis instead of treating the missing key as no mask.
cache_values["with-mask:MaskRequestHost"] = nil
host, port, mask = backend.resolve_backend("with-mask", "3000")
assert(host == "192.168.0.10")
assert(port == "3000")
assert(mask == "localhost:${PORT}")
assert(redis_calls == 2)

-- A genuinely absent mask is represented by boolean false, so warm requests
-- remain cache hits while decoding the value back to nil.
host, port, mask = backend.resolve_backend("without-mask", "3000")
assert(host == "192.168.0.10")
assert(port == "3000")
assert(mask == nil)
assert(cache_values["without-mask:MaskRequestHost"] == false)
assert(redis_calls == 3)

host, port, mask = backend.resolve_backend("without-mask", "3000")
assert(host == "192.168.0.10")
assert(port == "3000")
assert(mask == nil)
assert(redis_calls == 3)

print("sandbox_backend cache tests passed")
