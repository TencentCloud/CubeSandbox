package.path = "./lua/?.lua;" .. package.path

local cache_values = {}
local cache_set_counts = {}
local cache = {}

function cache:get(key)
    return cache_values[key]
end

function cache:set(key, value, _ttl)
    cache_values[key] = value
    cache_set_counts[key] = (cache_set_counts[key] or 0) + 1
    return true
end

function cache:add(key, value, _ttl)
    if cache_values[key] ~= nil then
        return false, "exists"
    end
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
local endpoint_hget_calls = 0
local endpoint_hget_err = nil
local node_endpoints = {
    ["node-dynamic"] = "10.0.0.50",
}
-- Mutable so a test can simulate Resume rewriting the backend (SandboxIP).
local current_backend_ip = "192.168.0.10"
package.loaded["redis_iresty"] = {
    new = function()
        return {
            hget = function(_, key, field)
                endpoint_hget_calls = endpoint_hget_calls + 1
                assert(key == "cube:v1:shared:node:endpoint", "unexpected endpoint key: " .. tostring(key))
                if endpoint_hget_err then
                    return nil, endpoint_hget_err
                end
                return node_endpoints[field], nil
            end,
            hgetall = function(_, key)
                redis_calls = redis_calls + 1
                local sandbox_id = key:match("([^:]+)$")
                -- Cross-host sandbox with HostID whose compute-node IP changes
                -- independently of the static sandbox proxy Hash.
                if sandbox_id == "dynamic-node" then
                    return {
                        "HostID", "node-dynamic",
                        "HostIP", "10.0.0.50",
                        "SandboxIP", current_backend_ip,
                        "AllowPublicTraffic", "true",
                        "8080", "25001",
                    }, nil
                end
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

-- Dynamic compute-node IP change: sandbox metadata stays cached while
-- node_endpoint:{HostID} expires after its short fixed TTL and resolves to
-- the compute node's new IP without purging per-sandbox caches.
redis_calls = 0
endpoint_hget_calls = 0
local dh1, dp1 = backend.resolve_backend("dynamic-node", "8080")
assert(dh1 == "10.0.0.50" and dp1 == "25001",
    "initial dynamic-node route = " .. tostring(dh1) .. ":" .. tostring(dp1))
assert(redis_calls == 1 and endpoint_hget_calls == 1)

-- Warm hit reuses cached node_endpoint without hitting Redis again.
local dh2, dp2 = backend.resolve_backend("dynamic-node", "8080")
assert(dh2 == "10.0.0.50" and dp2 == "25001")
assert(redis_calls == 1 and endpoint_hget_calls == 1)

-- Simulate BigPod restart with a new Pod IP and short TTL expiry of only the
-- node endpoint key (the sandbox route entry itself stays cached).
node_endpoints["node-dynamic"] = "10.0.0.99"
cache_values["node_endpoint:node-dynamic"] = nil
local dh3, dp3 = backend.resolve_backend("dynamic-node", "8080")
assert(dh3 == "10.0.0.99" and dp3 == "25001",
    "must converge to new compute-node IP, got " .. tostring(dh3) .. ":" .. tostring(dp3))
assert(redis_calls == 1, "sandbox route must stay a cache hit when only node IP changes")
assert(endpoint_hget_calls == 2, "only the node endpoint entry should be reloaded")

-- Transient Redis error after node IP change must:
--  1) keep routing to the last known live IP (10.0.0.99), NOT stale HostIP (10.0.0.50);
--  2) write a short backoff entry so subsequent requests do not hammer Redis;
--  3) pick up the new IP once backoff expires and Redis recovers.
cache_values["node_endpoint:node-dynamic"] = nil
endpoint_hget_err = "timeout"
local last_sets_before_err = cache_set_counts["node_endpoint_last:node-dynamic"] or 0
local dh_err1, dp_err1 = backend.resolve_backend("dynamic-node", "8080")
assert(dh_err1 == "10.0.0.99" and dp_err1 == "25001",
    "Redis error must fall back to last known live IP, got " .. tostring(dh_err1))
assert(endpoint_hget_calls == 3)
assert((cache_set_counts["node_endpoint_last:node-dynamic"] or 0) == last_sets_before_err + 1,
    "Redis error must renew node_endpoint_last TTL so extended outages do not expire it")
local dh_err2, dp_err2 = backend.resolve_backend("dynamic-node", "8080")
assert(dh_err2 == "10.0.0.99" and dp_err2 == "25001")
assert(endpoint_hget_calls == 3, "error backoff must prevent per-request Redis storms")

-- Concurrent refresh lock held by another worker serves last known live IP
-- without issuing a duplicate Redis HGET.
cache_values["node_endpoint:node-dynamic"] = nil
cache_values["node_endpoint_lock:node-dynamic"] = true
endpoint_hget_err = nil
local dh_lock, dp_lock = backend.resolve_backend("dynamic-node", "8080")
assert(dh_lock == "10.0.0.99" and dp_lock == "25001",
    "in-flight refresh must serve last known live IP, got " .. tostring(dh_lock))
assert(endpoint_hget_calls == 3, "in-flight refresh lock must suppress duplicate HGET")
cache_values["node_endpoint_lock:node-dynamic"] = nil

-- Once backoff/lock clears and Redis recovers, the new IP takes effect.
node_endpoints["node-dynamic"] = "10.0.0.88"
local dh_rec, dp_rec = backend.resolve_backend("dynamic-node", "8080")
assert(dh_rec == "10.0.0.88" and dp_rec == "25001",
    "recovery must pick up live IP, got " .. tostring(dh_rec))
assert(endpoint_hget_calls == 4)

-- Genuine Redis miss (no entry in node:endpoint) falls back to static HostIP.
node_endpoints["node-dynamic"] = nil
cache_values["node_endpoint:node-dynamic"] = nil
local dh4, dp4 = backend.resolve_backend("dynamic-node", "8080")
assert(dh4 == "10.0.0.50" and dp4 == "25001",
    "missing endpoint must fall back to static HostIP, got " .. tostring(dh4))

print("sandbox_backend cache tests passed")
