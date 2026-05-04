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
    1 arg:
        - backend_ip: the backend to check
        - check_local: whether check the local cache first
    2 return values:
        - true if redis mark the backend is faulty, false otherwise
        - error: any error that occurred during executing the function
--]]
function _M.is_faulty_backend(self, backend_ip, check_remote)
    local cache = ngx.shared.faulty_backend
    local value = cache:get(backend_ip)
    if not value or value ~= "true" then
        return false, nil
    end

    if check_remote == false then
        return true, nil
    end

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

    return res == 1, nil
end

return _M
