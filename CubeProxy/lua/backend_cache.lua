-- backend_cache.lua
--
-- Helpers for the per-sandbox routing cache in ngx.shared.local_cache
-- (see sandbox_backend.lua). Keys are prefixed with "{sandbox_id}:".

local _M = {}

local ROUTE_CACHE_SUFFIXES = {
    "meta_cached",
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

-- Invalidate the cache-hit sentinel first, then remove the fixed route metadata
-- mirrored from Redis. Dynamic per-port entries are intentionally left to their
-- existing TTL so invalidation stays bounded and never scans the shared dict.
-- Returns the number of fixed keys that existed. Safe when the dict is missing.
function _M.invalidate_sandbox(sandbox_id)
    if type(sandbox_id) ~= "string" or sandbox_id == "" then
        return 0
    end
    local cache = cache_dict()
    if not cache then
        return 0
    end

    local deleted = 0
    local prefix = sandbox_id .. ":"
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
