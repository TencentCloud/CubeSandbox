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

return _M
