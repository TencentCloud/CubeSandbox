-- backend_cache.lua
--
-- Helpers for the per-sandbox routing cache in ngx.shared.local_cache
-- (see sandbox_backend.lua). Keys are prefixed with "{sandbox_id}:".

local _M = {}

local function cache_dict()
    return ngx.shared.local_cache
end

-- delete_sandbox removes every local_cache entry belonging to sandbox_id.
-- Returns the number of keys deleted. Safe when the dict is missing.
function _M.delete_sandbox(sandbox_id)
    if type(sandbox_id) ~= "string" or sandbox_id == "" then
        return 0
    end
    local cache = cache_dict()
    if not cache then
        return 0
    end

    local prefix = sandbox_id .. ":"
    local deleted = 0
    -- get_keys(0) returns at most 1024 keys; loop until a pass finds none
    -- matching this sandbox so large dicts still converge.
    for _ = 1, 32 do
        local keys = cache:get_keys(0)
        local n = 0
        for _, k in ipairs(keys) do
            if type(k) == "string" and k:sub(1, #prefix) == prefix then
                cache:delete(k)
                deleted = deleted + 1
                n = n + 1
            end
        end
        if n == 0 then
            break
        end
    end
    return deleted
end

return _M
