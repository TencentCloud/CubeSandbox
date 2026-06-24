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

-- enforce_traffic_token rejects (403) requests targeting a sandbox whose
-- AllowPublicTraffic flag is "false" unless the request carries a matching
-- token in either the e2b-traffic-access-token (E2B-compatible) or
-- cube-traffic-access-token (CubeSandbox-native) header.
--
-- Both args are the raw values stored in Redis (string form). expected_token
-- being empty while allow_public is "false" indicates a server-side
-- inconsistency and yields 500 instead of 403, on the principle that
-- silently letting the request through would be worse.
local function enforce_traffic_token(allow_public, expected_token, ins_id)
    if allow_public ~= "false" then
        return
    end
    if utils:is_null(expected_token) then
        ngx.log(ngx.ERR, "LEVEL_ERROR||",
            string.format("request %s sandbox %s marked restricted but token missing in metadata",
                ngx.var.http_x_cube_request_id, ins_id))
        ngx.var.cube_retcode = "310507"
        ngx.exit(500)
    end
    local provided = ngx.var.http_e2b_traffic_access_token
                  or ngx.var.http_cube_traffic_access_token
    if not provided or provided ~= expected_token then
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s sandbox %s traffic token mismatch",
                ngx.var.http_x_cube_request_id, ins_id))
        ngx.var.cube_retcode = "310403"
        ngx.exit(ngx.HTTP_FORBIDDEN)
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

local function load_sandbox_proxy_metadata(ins_id)
    local redis = require "redis_iresty"
    local red = redis:new({
        redis_ip = ngx.var.redis_ip,
        redis_port = ngx.var.redis_port,
        redis_pd = ngx.var.redis_pd,
        redis_index = ngx.var.redis_index
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
        else
            -- This Redis command succeeded but the key is empty/missing. Clear
            -- any previous key's transport error so the final result is a
            -- truthful "not found" instead of a stale connectivity error.
            last_err = nil
        end
    end

    if last_err then
        return nil, string.format("request %s using keys for %s get redis err: %s",
            ngx.var.http_x_cube_request_id, ins_id, last_err)
    end
    return nil, string.format("request %s using keys for %s get redis nil",
        ngx.var.http_x_cube_request_id, ins_id)
end

--[[
    Resolve the upstream backend for a sandbox + container port.

    2 args:
        - ins_id: string, sandbox / instance id
        - container_port: string, e.g. "8080" or "32000"
    2 return values:
        - host_ip: string
        - host_port: string

    On unrecoverable error this function calls ngx.exit() and does not return.
--]]
function _M.resolve_backend(ins_id, container_port)
    local caller_host_ip = get_caller_host_ip()
    local cache = ngx.shared.local_cache
    local timeout = get_cache_timeout()
    local cache_backend_ip_key = string.format("%s:%s:%s", ins_id, container_port, "backend_ip")
    local cache_backend_port_key = string.format("%s:%s:%s", ins_id, container_port, "backend_port")
    local host_ip = cache:get(cache_backend_ip_key)
    local host_port = cache:get(cache_backend_port_key)
    if host_ip and host_port
        and cache:get(ins_id .. ":meta_cached") then
        -- Cache-hit path must still enforce the per-sandbox traffic token,
        -- otherwise a single warm entry would let unauthenticated callers
        -- bypass the gate for the whole cache TTL. The meta_cached sentinel
        -- shares the TTL of the auth fields; if it is absent the auth
        -- metadata has expired (or predates this feature), so fall through
        -- to the Redis reload below instead of trusting a nil that may just
        -- mean "expired". Refresh the auth keys alongside the backend keys
        -- so their TTLs never drift apart under steady traffic.
        local allow_public = cache:get(ins_id .. ":AllowPublicTraffic")
        local traffic_token = cache:get(ins_id .. ":TrafficAccessToken")
        enforce_traffic_token(allow_public, traffic_token, ins_id)

        cache:set(ins_id .. ":meta_cached", "1", timeout)
        cache:set(cache_backend_ip_key, host_ip, timeout)
        cache:set(cache_backend_port_key, host_port, timeout)
        cache:set(ins_id .. ":AllowPublicTraffic", allow_public, timeout)
        cache:set(ins_id .. ":TrafficAccessToken", traffic_token, timeout)
        return host_ip, host_port
    end

    local metadata, err = load_sandbox_proxy_metadata(ins_id)
    if err then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", err)
        ngx.var.cube_retcode = "310500"
        ngx.exit(500)
    end

    cache:set(ins_id .. ":meta_cached", "1", timeout)
    local metadata_map = {}
    for i = 1, #metadata, 2 do
        local k = metadata[i]
        local v = metadata[i + 1]
        metadata_map[k] = v
        cache:set(ins_id .. ":" .. k, v, timeout)
    end

    -- Restrict Public Access: gate the request before exposing any backend
    -- info. Legacy entries written before this feature have no
    -- AllowPublicTraffic field, which evaluates as nil here and therefore
    -- skips enforcement (publicly reachable, the historical default).
    enforce_traffic_token(
        metadata_map["AllowPublicTraffic"],
        metadata_map["TrafficAccessToken"],
        ins_id)

    local target_host_ip = metadata_map["HostIP"]
    local target_sandbox_ip = metadata_map["SandboxIP"]
    if utils:is_null(target_host_ip) then
        ngx.log(ngx.ERR, "LEVEL_WARN||",
            string.format("request %s using instance %s misses HostIP",
                ngx.var.http_x_cube_request_id, ins_id))
        ngx.var.cube_retcode = "310507"
        ngx.exit(500)
    end

    if not utils:is_null(caller_host_ip) and caller_host_ip == target_host_ip then
        if utils:is_null(target_sandbox_ip) then
            ngx.log(ngx.ERR, "LEVEL_ERROR||",
                string.format("request %s instance %s on local host %s misses SandboxIP",
                    ngx.var.http_x_cube_request_id, ins_id, caller_host_ip))
            ngx.var.cube_retcode = "310507"
            ngx.exit(500)
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
            ngx.var.cube_retcode = "310507"
            ngx.exit(500)
        end
    end

    cache:set(cache_backend_ip_key, host_ip, timeout)
    cache:set(cache_backend_port_key, host_port, timeout)
    return host_ip, host_port
end

return _M
