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
    Terminates the request with a uniform JSON body and status. Used for all
    client-facing error paths so that no implementation detail (which
    subsystem failed, whether the sandbox exists, lifecycle state, etc.) is
    distinguishable to the caller. The detailed reason stays in error.log
    via the preceding ngx.log call.

    2 args:
        - status: HTTP status code to exit with
        - body:   response body string (JSON)
--]]
function _M.respond_with(self, status, body)
    ngx.status = status
    ngx.header["Content-Type"] = "application/json"
    ngx.say(body)
    ngx.exit(status)
end

-- Convenience wrappers for the three error shapes the dataplane returns.
-- 400 = malformed request; 404 hides sandbox existence; 503 hides the
-- specific failing subsystem.
function _M.respond_bad_request(self)
    self:respond_with(400, '{"error":"bad request"}')
end

function _M.respond_not_found(self)
    self:respond_with(404, '{"error":"not found"}')
end

function _M.respond_unavailable(self)
    self:respond_with(503, '{"error":"service unavailable"}')
end

return _M
