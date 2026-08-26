-- file name: sandbox_backend.lua
--
-- Shared backend resolution helpers used by both host-based and path-based
-- sandbox routing entry points (rewrite_phase.lua / path_rewrite_phase.lua).
--
-- Looks up the sandbox proxy metadata stored in Redis under
-- "bypass_host_proxy:<sandbox_id>" (written by CubeMaster) and returns the
-- upstream host:port that the nginx balancer phase should connect to.

local utils = require "utils"
local redis_keys = require "redis_keys"

local _M = { _VERSION = "0.01" }

-- enforce_traffic_token rejects requests targeting a sandbox whose
-- AllowPublicTraffic flag is "false" unless the request carries a matching
-- token in either the e2b-traffic-access-token (E2B-compatible) or
-- cube-traffic-access-token (CubeSandbox-native) header.
--
-- Both args are the raw values stored in Redis (string form). expected_token
-- being empty while allow_public is "false" indicates a server-side
-- inconsistency. Both failure paths return 403 (not the collapsed 404 used
-- for "sandbox not found") to preserve E2B behavioral compatibility — E2B's
-- restrict-public-access contract, our public docs, and the SDK all promise
-- that unauthenticated / mismatched tokens are rejected with 403. This does
-- leak the fact that the sandbox exists (vs. non-existent → 404), but the
-- sandbox ID space is already unguessable and the wire compatibility win
-- outweighs the enumeration signal.
local function enforce_traffic_token(allow_public, expected_token, ins_id)
    if allow_public ~= "false" then
        return
    end
    if utils:is_null(expected_token) then
        ngx.log(ngx.ERR, "LEVEL_ERROR||",
            string.format("request %s sandbox %s marked restricted but token missing in metadata",
                ngx.var.http_x_cube_request_id, ins_id))
        utils:respond_forbidden()
    end
    local provided = ngx.var.http_e2b_traffic_access_token
                  or ngx.var.http_cube_traffic_access_token
    if not provided or provided ~= expected_token then
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s sandbox %s traffic token mismatch",
                ngx.var.http_x_cube_request_id, ins_id))
        utils:respond_forbidden()
    end
end

local function get_cache_timeout()
    return math.random(tonumber(ngx.var.timeout_min), tonumber(ngx.var.timeout_max))
end

local function get_caller_host_ip()
    if not utils:is_null(ngx.var.cube_proxy_host_ip) then
        return ngx.var.cube_proxy_host_ip
    end
    if not utils:is_null(ngx.var.server_addr) then
        return ngx.var.server_addr
    end
    return ""
end

-- Optional metadata must still have a cache entry when absent. ngx.shared.DICT
-- returns nil both for "not cached" and "evicted"; boolean false is therefore
-- used as an unambiguous negative-cache value (Redis values are strings).
local function encode_optional_cache_value(value)
    if utils:is_null(value) then
        return false
    end
    return value
end

local function decode_optional_cache_value(value)
    if value == false then
        return nil
    end
    return value
end

local function load_sandbox_proxy_metadata(ins_id)
    local redis = require "redis_iresty"
    local red = redis:new({
        redis_ip = ngx.var.redis_ip,
        redis_port = ngx.var.redis_port,
        redis_pd = ngx.var.redis_pd,
        redis_index = ngx.var.redis_index,
        redis_master_name = ngx.var.redis_master_name,
        redis_sentinel_nodes = ngx.var.redis_sentinel_nodes,
        redis_sentinel_pd = ngx.var.redis_sentinel_pd,
    })

    -- During migration we try the new namespaced key first and fall back to the
    -- legacy "bypass_host_proxy:<id>" key.
    local keys = redis_keys.read_keys_with_fallback(
        redis_keys.sandbox_proxy(ins_id),
        redis_keys.legacy_sandbox_proxy(ins_id))

    local last_err
    for _, key in ipairs(keys) do
        local value, err
        for i = 1, 3 do
            value, err = red:hgetall(key)
            if not err then
                break
            end
            ngx.log(ngx.ERR, "LEVEL_WARN||",
                string.format("request %s using key %s get redis err: %s, retry %d",
                    ngx.var.http_x_cube_request_id, key, err, i))
        end
        if err then
            last_err = err
        elseif value and #value > 0 then
            return value, nil
        end
    end

    if last_err then
        return nil, string.format("request %s using keys for %s get redis err: %s",
            ngx.var.http_x_cube_request_id, ins_id, last_err)
    end
    return nil, nil
end

-- Read path. Returns host_ip, host_port, mask_request_host on a valid hit,
-- nil on a miss. The completeness gate is the presence of EVERY served field
-- (false is a valid negative-cache sentinel; only nil means missing): a
-- partial fill or a field lost to TTL skew / LRU eviction leaves a nil and
-- forces a reload instead of serving an incomplete entry or silently skipping
-- the traffic-token check. There is no separate meta_cached flag — checking
-- the fields directly makes one redundant.
--
-- The serving backend is computed from the raw route fields the fill mirrors
-- for the whole sandbox (HostIP / SandboxIP / the per-port host-port mapping),
-- resolved for the caller's host. A single fill writes every port's mapping at
-- once, so after an invalidate all ports converge on fresh data from one
-- reload — no per-port reload, no route epoch. On a hit the TTLs are refreshed.
local function read_cached_backend(cache, ins_id, container_port, caller_host_ip, timeout)
    -- The presence of every served field is the completeness gate; there is no
    -- separate meta_cached flag. A partial fill or a field lost to TTL skew /
    -- LRU eviction leaves a nil here and forces a reload instead of serving an
    -- incomplete entry (or skipping the traffic-token check).
    local target_host_ip = cache:get(ins_id .. ":HostIP")
    local target_sandbox_ip = cache:get(ins_id .. ":SandboxIP")
    local mapped_port = cache:get(ins_id .. ":" .. container_port)
    local allow_public = cache:get(ins_id .. ":AllowPublicTraffic")
    local traffic_token = cache:get(ins_id .. ":TrafficAccessToken")
    local mask_request_host = cache:get(ins_id .. ":MaskRequestHost")

    if not (target_host_ip and target_sandbox_ip
            and allow_public ~= nil and traffic_token ~= nil
            and mask_request_host ~= nil) then
        return nil
    end

    -- Same-host callers dial the sandbox IP + container port directly; only a
    -- cross-host caller needs the mapped host port.
    local same_host = not utils:is_null(caller_host_ip) and caller_host_ip == target_host_ip
    if not same_host and mapped_port == nil then
        return nil
    end

    -- The cache-hit path must still enforce the per-sandbox traffic token,
    -- otherwise a single warm entry would let unauthenticated callers bypass
    -- the gate for the whole cache TTL.
    enforce_traffic_token(decode_optional_cache_value(allow_public),
                          decode_optional_cache_value(traffic_token), ins_id)

    local host_ip, host_port
    if same_host then
        host_ip = target_sandbox_ip
        host_port = container_port
    else
        host_ip = target_host_ip
        host_port = mapped_port
    end

    cache:set(ins_id .. ":HostIP", target_host_ip, timeout)
    cache:set(ins_id .. ":SandboxIP", target_sandbox_ip, timeout)
    if not same_host then
        cache:set(ins_id .. ":" .. container_port, mapped_port, timeout)
    end
    cache:set(ins_id .. ":AllowPublicTraffic", allow_public, timeout)
    cache:set(ins_id .. ":TrafficAccessToken", traffic_token, timeout)
    cache:set(ins_id .. ":MaskRequestHost", mask_request_host, timeout)

    return host_ip, host_port, decode_optional_cache_value(mask_request_host)
end

--[[
    Resolve the upstream backend for a sandbox + container port.

    2 args:
        - ins_id: string, sandbox / instance id
        - container_port: string, e.g. "8080" or "32000"
    3 return values:
        - host_ip: string
        - host_port: string
        - mask_request_host: optional unexpanded Host template

    On unrecoverable error this function calls ngx.exit() and does not return.
--]]
function _M.resolve_backend(ins_id, container_port)
    local caller_host_ip = get_caller_host_ip()
    local cache = ngx.shared.local_cache
    local timeout = get_cache_timeout()

    local host_ip, host_port, mask_request_host =
        read_cached_backend(cache, ins_id, container_port, caller_host_ip, timeout)
    if host_ip then
        return host_ip, host_port, mask_request_host
    end

    local metadata, err = load_sandbox_proxy_metadata(ins_id)
    if err then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", err)
        utils:respond_unavailable()
    end
    if not metadata then
        ngx.log(ngx.WARN, "LEVEL_WARN||",
            string.format("request %s using instance %s has no Redis route",
                ngx.var.http_x_cube_request_id, ins_id))
        utils:respond_not_found()
    end

    -- Fill path: mirror every metadata key (including every port's host-port
    -- mapping), so one fill refreshes the whole sandbox's route. The read gate
    -- then sees every served field present, which is the completeness signal.
    local metadata_map = {}
    for i = 1, #metadata, 2 do
        local k = metadata[i]
        local v = metadata[i + 1]
        metadata_map[k] = v
        cache:set(ins_id .. ":" .. k, v, timeout)
    end
    -- Optional fields must be re-written with encode_optional_cache_value so a
    -- missing Redis key becomes an explicit false negative-cache sentinel
    -- (ngx.shared.DICT returns nil for both "missing" and "evicted").
    local allow_public = metadata_map["AllowPublicTraffic"]
    local traffic_token = metadata_map["TrafficAccessToken"]
    local mask_request_host = metadata_map["MaskRequestHost"]
    cache:set(ins_id .. ":AllowPublicTraffic", encode_optional_cache_value(allow_public), timeout)
    cache:set(ins_id .. ":TrafficAccessToken", encode_optional_cache_value(traffic_token), timeout)
    cache:set(ins_id .. ":MaskRequestHost", encode_optional_cache_value(mask_request_host), timeout)

    -- Restrict Public Access: gate the request before exposing any backend
    -- info. Legacy entries written before this feature have no
    -- AllowPublicTraffic field, which evaluates as nil here and therefore
    -- skips enforcement (publicly reachable, the historical default).
    enforce_traffic_token(
        allow_public,
        traffic_token,
        ins_id)

    local target_host_ip = metadata_map["HostIP"]
    local target_sandbox_ip = metadata_map["SandboxIP"]
    if utils:is_null(target_host_ip) then
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s using instance %s misses HostIP",
                ngx.var.http_x_cube_request_id, ins_id))
        utils:respond_not_found()
    end

    if not utils:is_null(caller_host_ip) and caller_host_ip == target_host_ip then
        if utils:is_null(target_sandbox_ip) then
            ngx.log(ngx.ERR, "LEVEL_ERROR||",
                string.format("request %s instance %s on local host %s misses SandboxIP",
                    ngx.var.http_x_cube_request_id, ins_id, caller_host_ip))
            utils:respond_not_found()
        end
        host_ip = target_sandbox_ip
        host_port = container_port
    else
        host_ip = target_host_ip
        host_port = metadata_map[container_port]
        if utils:is_null(host_port) then
            ngx.log(ngx.ERR, "LEVEL_ERROR||",
                string.format("request %s instance %s misses host port mapping for container_port %s",
                    ngx.var.http_x_cube_request_id, ins_id, container_port))
            utils:respond_not_found()
        end
    end

    -- The serving backend is recomputed from the mirrored raw fields on the
    -- read path, so there is no separate resolved-backend key to write here.
    return host_ip, host_port, mask_request_host
end

return _M
