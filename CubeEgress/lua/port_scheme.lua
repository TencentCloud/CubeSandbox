-- CubeEgress L7 destination port/scheme semantics shared by validation and
-- request matching.
local _M = {}

local function trim(s)
    return (string.gsub(s, "^%s*(.-)%s*$", "%1"))
end

function _M.normalize_scheme(value)
    if value == nil then return nil end
    if type(value) ~= "string" then
        return nil, "scheme must be a string"
    end
    local scheme = string.lower(trim(value))
    if scheme ~= "http" and scheme ~= "https" then
        return nil, "scheme must be http or https"
    end
    return scheme
end

local function normalize_port(value)
    if value == nil then return nil end
    if type(value) ~= "number" or value ~= math.floor(value) then
        return nil, "port must be an integer"
    end
    if value < 1 or value > 65535 then
        return nil, "port must be in [1, 65535]"
    end
    return value
end

-- expand returns the effective destination tuples for a rule:
--   nil/nil      -> 80/http + 443/https
--   nil/scheme   -> the scheme's conventional port
--   port/scheme  -> the exact tuple
--   port/nil     -> invalid
function _M.expand(port_value, scheme_value)
    local port, perr = normalize_port(port_value)
    if perr then return nil, perr end
    local scheme, serr = _M.normalize_scheme(scheme_value)
    if serr then return nil, serr end

    if port ~= nil and scheme == nil then
        return nil, "port requires scheme"
    end
    if port ~= nil then
        return {{port = port, scheme = scheme}}
    end
    if scheme == "http" then
        return {{port = 80, scheme = "http"}}
    end
    if scheme == "https" then
        return {{port = 443, scheme = "https"}}
    end
    return {
        {port = 80, scheme = "http"},
        {port = 443, scheme = "https"},
    }
end

function _M.matches(port_value, scheme_value, dst_port, request_scheme)
    local tuples = _M.expand(port_value, scheme_value)
    if not tuples then return false end
    local scheme = _M.normalize_scheme(request_scheme)
    local port = tonumber(dst_port)
    if not scheme or not port then return false end

    for _, tuple in ipairs(tuples) do
        if tuple.port == port and tuple.scheme == scheme then
            return true
        end
    end
    return false
end

return _M
