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

function cache:delete(key)
    cache_values[key] = nil
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
-- Mutable so a test can simulate Resume rewriting the backend (SandboxIP).
local current_backend_ip = "192.168.0.10"
package.loaded["redis_iresty"] = {
    new = function()
        return {
            hgetall = function(_, key)
                redis_calls = redis_calls + 1
                local sandbox_id = key:match("([^:]+)$")
                -- Cross-host sandbox: HostIP differs from the caller (10.0.0.1),
                -- with per-port host-port mappings.
                if sandbox_id == "cross-o1" then
                    return {
                        "HostIP", "10.0.0.9",
                        "SandboxIP", current_backend_ip,
                        "AllowPublicTraffic", "true",
                        "49999", "30001",
                        "49983", "30002",
                    }, nil
                end
                local values = {
                    "HostIP", "10.0.0.1",
                    "SandboxIP", current_backend_ip,
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
local backend_cache = require "backend_cache"

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

-- O1 regression: a multi-port sandbox that is Resumed (backend changes) then
-- invalidated must converge EVERY port on the new backend from a single
-- reload. One fill mirrors the whole sandbox's route fields, so port B hits
-- the fresh entry written by port A's reload instead of serving its stale
-- value.
redis_calls = 0
local h1 = backend.resolve_backend("multi-o1", "49999") -- fill port A (old backend)
assert(h1 == "192.168.0.10")
local h2 = backend.resolve_backend("multi-o1", "49983") -- port B hits port A's fill
assert(h2 == "192.168.0.10")
assert(redis_calls == 1, "one fill must refresh every port, so port B is a hit")

current_backend_ip = "192.168.0.20" -- Resume rewrites the backend
backend_cache.invalidate_sandbox("multi-o1")

h1 = backend.resolve_backend("multi-o1", "49999") -- miss -> reload -> new backend
assert(h1 == "192.168.0.20", "port A must reload to the new backend")
assert(redis_calls == 2)

-- The whole point: port B must NOT serve its stale value. port A's reload
-- mirrored the new backend for the whole sandbox, so port B hits the fresh
-- entry rather than its old one.
h2 = backend.resolve_backend("multi-o1", "49983")
assert(h2 == "192.168.0.20",
    "port B must serve the new backend refreshed by port A's fill, got " .. tostring(h2))
assert(redis_calls == 2, "port B must hit the refreshed entry, not reload")

-- Both ports stay warm.
h1 = backend.resolve_backend("multi-o1", "49999")
h2 = backend.resolve_backend("multi-o1", "49983")
assert(h1 == "192.168.0.20" and h2 == "192.168.0.20")
assert(redis_calls == 2, "warm ports must be cache hits")

-- Cross-host: the serving backend is HostIP + the per-port mapped host port,
-- read from the raw mapping the fill mirrors. One fill mirrors every port's
-- mapping, so a second port is a cache hit without reloading.
redis_calls = 0
local ch1, cp1 = backend.resolve_backend("cross-o1", "49999")
assert(ch1 == "10.0.0.9" and cp1 == "30001",
    "cross-host must serve HostIP:mapped_port, got " .. tostring(ch1) .. ":" .. tostring(cp1))
local ch2, cp2 = backend.resolve_backend("cross-o1", "49983")
assert(ch2 == "10.0.0.9" and cp2 == "30002",
    "cross-host port B must serve its own mapped_port, got " .. tostring(ch2) .. ":" .. tostring(cp2))
assert(redis_calls == 1, "one fill mirrors every port's mapping")

-- Completeness gate: a partial set of fields must not produce a hit. Setting
-- only HostIP (without SandboxIP / the access fields) forces a reload.
redis_calls = 0
cache_values["partial:HostIP"] = "10.0.0.1"
backend.resolve_backend("partial", "3000")
assert(redis_calls == 1, "an incomplete field set must be a miss, not a hit")

-- Eviction resilience: losing the token field on a hit must reload, not skip.
redis_calls = 0
backend.resolve_backend("evict-token", "3000") -- fill
assert(redis_calls == 1)
cache_values["evict-token:TrafficAccessToken"] = nil
backend.resolve_backend("evict-token", "3000")
assert(redis_calls == 2, "evicted token field must force a reload, not a hit")

print("sandbox_backend cache tests passed")
