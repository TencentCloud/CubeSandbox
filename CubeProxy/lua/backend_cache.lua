-- backend_cache.lua
--
-- Helpers for the per-sandbox routing cache in ngx.shared.local_cache
-- (see sandbox_backend.lua). Keys are prefixed with "{sandbox_id}:".

local _M = {}

-- Fixed per-sandbox route metadata mirrored from Redis. The per-port host-port
-- mappings ({sid}:{container_port}) are also served from the cache but are
-- gated by HostIP/SandboxIP and overwritten on the next fill, so invalidation
-- only needs to drop these fixed keys.
local ROUTE_CACHE_SUFFIXES = {
    "HostIP",
    "SandboxIP",
    "CreatedAt",
    "AllowPublicTraffic",
    "TrafficAccessToken",
    "MaskRequestHost",
}

local function cache_dict()
    return ngx.shared.local_cache
end

-- Invalidate this sandbox's cached route by deleting the fixed route metadata
-- mirrored from Redis. The next read misses (HostIP/SandboxIP gone) and a
-- single fill rewrites every port's mapping at once, so all ports converge on
-- fresh data from one reload. Returns the number of fixed keys that existed.
-- Safe when the dict is missing.
function _M.invalidate_sandbox(sandbox_id)
    if type(sandbox_id) ~= "string" or sandbox_id == "" then
        return 0
    end
    local cache = cache_dict()
    if not cache then
        return 0
    end

    local prefix = sandbox_id .. ":"

    local deleted = 0
    for _, suffix in ipairs(ROUTE_CACHE_SUFFIXES) do
        local key = prefix .. suffix
        if cache:get(key) ~= nil then
            deleted = deleted + 1
        end
        cache:delete(key)
    end
    return deleted
end

return _M
