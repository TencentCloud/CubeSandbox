local ok, new_tab = pcall(require, "table.new")
if not ok or type(new_tab) ~= "function" then
    new_tab = function(narr, nrec)
        return {}
    end
end

local _M = new_tab(0, 10)
_M._VERSION = '0.01'

-- HTTP status → gRPC status code (google.rpc.Code). Used on the plaintext
-- gRPC ingress so native clients get trailers instead of JSON over HTTP 4xx/5xx.
local GRPC_STATUS = {
    [400] = 3,  -- INVALID_ARGUMENT
    [403] = 7,  -- PERMISSION_DENIED
    [404] = 5,  -- NOT_FOUND
    [410] = 9,  -- FAILED_PRECONDITION
    [503] = 14, -- UNAVAILABLE
}

local GRPC_MESSAGE = {
    [400] = "bad request",
    [403] = "forbidden",
    [404] = "not found",
    [410] = "gone",
    [503] = "unavailable",
}

--[[
    1 arg:
        - str: the string to check
    1 return value:
        - true if the string is null or empty, false otherwise
--]]
function _M.is_null(self, str)
    return str == nil or str == ""
end

-- True only on the dedicated plaintext gRPC server (:9090 sets
-- $cube_ingress_protocol = "grpc"). Do not key off Content-Type: a client
-- sending application/grpc to :80/:443 must stay on the HTTP error path —
-- those servers lack @grpc_lua_error / $cube_grpc_*.
function _M.is_grpc_request(self)
    return ngx.var.cube_ingress_protocol == "grpc"
end

--[[
    Terminates the request with a uniform client-facing error.

    HTTP ingress: JSON body + matching HTTP status (unchanged).
    gRPC ingress: HTTP 200 + Content-Type application/grpc + grpc-status /
    grpc-message headers (trailers when there is no response body), per
    gRPC over HTTP/2. Do not ngx.say a JSON body on the gRPC path — that
    breaks framing for native clients.

    2 args:
        - status: HTTP status code to exit with (also selects gRPC mapping)
        - body:   response body string (JSON); ignored on the gRPC path
--]]
function _M.respond_with(self, status, body)
    if self:is_grpc_request() then
        local gs = GRPC_STATUS[status] or 2 -- UNKNOWN
        local msg = GRPC_MESSAGE[status] or "unknown"
        ngx.var.cube_grpc_status = tostring(gs)
        -- Message is ASCII-only today; escape_uri keeps the trailer safe if
        -- future mappings introduce reserved characters.
        ngx.var.cube_grpc_message = ngx.escape_uri(msg)
        -- Prefer internal redirect over ngx.exit from rewrite: the latter
        -- produces a DATA END_STREAM without trailers that grpcio rejects.
        return ngx.exec("@grpc_lua_error")
    end

    ngx.status = status
    ngx.header["Content-Type"] = "application/json"
    ngx.say(body)
    ngx.exit(status)
end

-- Convenience wrappers for the error shapes the dataplane returns.
-- 400 = malformed request; 403 = traffic-token gate rejection
-- (deliberately distinct from 404 to preserve E2B header/status
-- compatibility for restricted-public-access clients); 404 hides
-- sandbox existence; 503 hides the specific failing subsystem.
function _M.respond_bad_request(self)
    self:respond_with(400, '{"error":"bad request"}')
end

function _M.respond_forbidden(self)
    self:respond_with(403, '{"error":"forbidden"}')
end

function _M.respond_not_found(self)
    self:respond_with(404, '{"error":"not found"}')
end

function _M.respond_unavailable(self)
    self:respond_with(503, '{"error":"service unavailable"}')
end

function _M.respond_gone(self)
    self:respond_with(410, '{"error":"gone"}')
end

return _M
