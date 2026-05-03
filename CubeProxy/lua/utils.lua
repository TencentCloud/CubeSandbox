-- file name: utils.lua
local ok, new_tab = pcall(require, "table.new")
if not ok or type(new_tab) ~= "function" then
    new_tab = function(narr, nrec)
        return {}
    end
end

local _M = new_tab(0, 155)
_M._VERSION = '0.01'

local mt = {
    __index = _M
}

--[[
    1 arg:
        - file_name: the file to read
    2 return values:
        - content: the content of the file
        - error: any error that occurred during executing the function
--]]
function _M.get_file_content(self, file_name)
    local f, err = io.open(file_name, "r")
    if not f then
        return "", err
    end

    local content = f:read("*all")
    f:close()

    return content, nil
end

--[[
    1 arg:
        - str: the string to check
    1 return value:
        - true if the string is null or empty, false otherwise
--]]
function _M.is_null(self, str)
    return str == nil or str == ""
end

--[[
    Mimics golang.org/x/sync/singleflight Do() interface.
    Since OpenResty workers are separate processes, we use resty.lock for cross-worker synchronization.
    The fn() provided MUST implement a cache check (double-checked locking) as its first step.
    Returns: val, err, shared (boolean indicating if we waited for the lock)
]]
function _M.singleflight_do(self, lock_dict_name, key, fn)
    local resty_lock = require "resty.lock"
    local lock, err = resty_lock:new(lock_dict_name, {
        exptime = 10,
        timeout = 5
    })
    if not lock then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", "failed to create singleflight lock: ", err)
        return nil, err, false
    end

    local elapsed, err = lock:lock(key)
    if not elapsed then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", "failed to acquire singleflight lock: ", err)
        return nil, err, false
    end

    local shared = (elapsed > 0)
    local ok, res, func_err = xpcall(fn, debug.traceback)

    local unlock_ok, unlock_err = lock:unlock()
    if not unlock_ok and unlock_err ~= "unlocked" then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", "singleflight unlock failed: ", unlock_err)
    end

    if not ok then
        ngx.log(ngx.ERR, "LEVEL_ERROR||", "singleflight fn error: ", res)
        return nil, res, shared
    end

    return res, func_err, shared
end

--[[
    2 args:
        - backend_ip: the backend IP to check
        - check_remote: if true, query Redis on cache miss; if false, return false immediately
    2 return values:
        - true if the backend is marked faulty, false otherwise
        - error: any error that occurred during executing the function
--]]
function _M.is_faulty_backend(self, backend_ip, check_remote)
    local cache = ngx.shared.faulty_backend

    -- 1st check: fast path, no lock needed
    local value = cache:get(backend_ip)
    if value ~= nil then
        return value == "true", nil
    end

    -- network I/O is disabled in header_filter phase, return directly
    if check_remote == false then
        return false, nil
    end

    local fn = function()
        -- 2nd check: another concurrent request may have populated the cache while we waited
        local val = cache:get(backend_ip)
        if val ~= nil then
            return val == "true", nil
        end

        -- cache is still empty — we are the single flight, query Redis
        local redis = require "redis_iresty"
        local red = redis:new({
            redis_ip = ngx.var.redis_ip,
            redis_port = ngx.var.redis_port,
            redis_pd = ngx.var.redis_pd,
            redis_index = ngx.var.redis_index
        })
        local res, err = red:sismember("faulty_backend_set", backend_ip)
        if err then
            return false, err
        end

        local is_faulty = (res == 1)
        cache:set(backend_ip, is_faulty and "true" or "false", 5)
        return is_faulty, nil
    end

    local res, err, shared = self:singleflight_do("faulty_backend_locks", "faulty:" .. backend_ip, fn)
    return res, err
end

return _M
